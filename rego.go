package main

import (
	"flag"
	"fmt"
	"go/build"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/tools/go/buildutil"
)

var (
	buildTags = flag.String("tags", "", buildutil.TagsFlagDoc)
	verbose   = flag.Bool("v", false, "verbose output")
	timings   = flag.Bool("timings", false, "show timings")
	race      = flag.Bool("race", false, "build with Go race detector")
	ienv      = flag.String("installenv", "", "env vars to pass to `go install` (comma-separated: A=B,C=D)")
	wdir      = flag.String("workdir", "", "working dir to locate the main module and run `go install`")
	extra     = flag.String("extra-watches", "", "comma-separated path match patterns to also watch (in addition to transitive deps of Go pkg)")
)

func main() {
	log.SetFlags(0)
	flag.Parse()

	if flag.NArg() == 0 {
		log.Fatal("must provide package path")
	}

	pkgPath := flag.Arg(0)
	cmdArgs := flag.Args()[1:]

	var installEnv []string
	if *ienv != "" {
		installEnv = append(os.Environ(), strings.Split(*ienv, ",")...)
	}

	var workingDir string
	if *wdir != "" {
		workingDir = *wdir
	} else {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	mainPkg, err := build.Import(pkgPath, workingDir, 0)
	if err != nil {
		log.Fatal(err)
	}

	if *verbose {
		log.Printf("Watching package %s", mainPkg.ImportPath)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	pkgs := []*build.Package{mainPkg}
	seenPkgs := map[string]struct{}{}
	for i := 0; i < len(pkgs); i++ {
		pkg := pkgs[i]
		if pkg.Goroot {
			continue // don't watch Go stdlib packages
		}
		if *verbose {
			log.Printf("Watch %s", pkg.Dir)
		}
		if err := w.Add(pkg.Dir); err != nil {
			log.Fatal(err)
		}

		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)
		for _, imp := range pkg.Imports {
			mu.Lock()
			_, seen := seenPkgs[imp]
			mu.Unlock()
			if seen {
				continue
			}

			if imp == "C" || strings.HasPrefix(imp, ".") {
				return
			}

			wg.Add(1)
			go func(imp string) {
				defer wg.Done()
				t0 := time.Now()
				impPkg, err := build.Import(imp, workingDir, 0)
				if err != nil {
					log.Fatal(err)
				}
				if *verbose {
					log.Printf("Import %s [%s]", imp, time.Since(t0))
				}
				mu.Lock()
				defer mu.Unlock()
				pkgs = append(pkgs, impPkg)
				seenPkgs[imp] = struct{}{}
			}(imp)
		}
		wg.Wait()
	}

	extraPaths := map[string]bool{}
	if *extra != "" {
		for _, pat := range strings.Split(*extra, ",") {
			matches, err := filepath.Glob(pat)
			if err != nil {
				log.Fatal(err)
			}
			for _, path := range matches {
				path, err = filepath.Abs(path)
				if err != nil {
					log.Fatal(err)
				}
				if *verbose {
					log.Printf("Watch (extra) %s", path)
				}
				if err := w.Add(path); err != nil {
					log.Fatal(err)
				}
				extraPaths[path] = true
			}
		}
	}

	restart := make(chan bool)
	go func() {
		var proc *os.Process
		for alive := range restart {
			if proc != nil {
				if err := proc.Signal(os.Interrupt); err != nil {
					log.Println(err)
					proc.Kill()
				}
				proc.Wait()
			}
			if !alive {
				os.Exit(0)
			}
			cmd := exec.Command(filepath.Join(mainPkg.BinDir, filepath.Base(mainPkg.ImportPath)), cmdArgs...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if *verbose {
				log.Println(cmd.Args)
			}
			if err := cmd.Start(); err != nil {
				log.Println(err)
			}
			proc = cmd.Process
		}
	}()

	go func() {
		go func() {
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			<-c
			restart <- false
		}()
	}()

	nrestarts := 0
	installAndRestart := func() {
		s := "\x1b[37;1m\x1b[44m .. \x1b[0m"
		del := len(s)
		fmt.Fprint(os.Stderr, s)

		cmd := exec.Command("go", "install", "-tags="+*buildTags)
		if *race {
			cmd.Args = append(cmd.Args, "-race")
		}
		cmd.Args = append(cmd.Args, mainPkg.ImportPath)
		cmd.Dir = workingDir
		cmd.Env = installEnv
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if *verbose {
			log.Println(cmd.Args)
			if installEnv != nil {
				log.Println("# with env:", installEnv)
			}
		}
		start := time.Now()
		if err := cmd.Run(); err == nil {
			var word string
			if nrestarts == 0 {
				word = "starting"
			} else {
				word = "restarting"
			}
			nrestarts++
			fmt.Fprint(os.Stderr, strings.Repeat("\b", del))
			log.Println("\x1b[37;1m\x1b[42m ok \x1b[0m", word)
			if *timings {
				log.Println("compilation took", time.Since(start))
			}
			restart <- true
		} else {
			log.Println("\x1b[37;1m\x1b[41m!!!!\x1b[0m", "compilation failed")
		}
	}

	install := make(chan struct{})
	go func() {
		needsInstall := 0
		for {
			var timerChan <-chan time.Time
			if needsInstall > 0 {
				timerChan = time.After(200 * time.Millisecond)
			} else {
				timerChan = make(chan time.Time) // never sent on, blocks indefinitely
			}
			select {
			case <-install:
				needsInstall++
				continue
			case <-timerChan:
				needsInstall = 0
				installAndRestart()
			}
		}
	}()
	install <- struct{}{}

	matchFile := func(name string) bool {
		return (filepath.Ext(name) == ".go" && !strings.HasPrefix(filepath.Base(name), ".")) || extraPaths[name]
	}

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				break
			}

			go func() {
				switch ev.Op {
				case fsnotify.Create, fsnotify.Rename, fsnotify.Write:
					paths := []string{ev.Name}

					// w.Add is non-recursive if the path is a dir, so
					// we need to scan for the files here.
					if fi, err := os.Stat(ev.Name); err != nil {
						if *verbose {
							log.Println(err)
						}
						return
					} else if fi.Mode().IsDir() {
						err := filepath.Walk(ev.Name, func(path string, info os.FileInfo, err error) error {
							if err != nil {
								if *verbose {
									log.Println(err)
								}
								return nil
							}
							if info.Mode().IsDir() || matchFile(info.Name()) {
								paths = append(paths, path)
							}
							return nil
						})
						if err != nil && *verbose {
							log.Println(err)
						}
					} else if !matchFile(ev.Name) {
						// File did not match.
						return
					}

					for _, path := range paths {
						if *verbose {
							log.Printf("Watch %s", path)
						}
						if err := w.Add(path); err != nil {
							if *verbose {
								log.Println(err)
							}
						}
					}

				case fsnotify.Remove:
					if err := w.Remove(ev.Name); err != nil {
						if *verbose {
							log.Println(err)
						}
					}
				case fsnotify.Chmod:
					return
				}
				if *verbose {
					log.Println(ev)
				}
				install <- struct{}{}
			}()
		case err, ok := <-w.Errors:
			if !ok {
				break
			}
			if ok {
				log.Fatal(err)
			}
		}
	}
}
