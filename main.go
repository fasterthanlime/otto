package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	humanize "github.com/dustin/go-humanize"

	"encoding/json"

	"net/http"

	"strings"

	"os/exec"

	"gopkg.in/alecthomas/kingpin.v2"
)

type Config struct {
	Profiles []*Profile
	Packages []*Package
}

type Profile struct {
	Name      string
	Env       map[string]string
	Configure []string
}

type Package struct {
	Name               string
	Sources            string
	Format             string
	Configure          []string
	ConfigureBlacklist []string
}

type Blacklist struct {
	Prefixes []string
}

func (bl *Blacklist) Has(s string) bool {
	for _, p := range bl.Prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var (
	app                 = kingpin.New("otto", "An autotools hater")
	configPath          = app.Arg("config", "Path to JSON config file").Required().String()
	outDirArg           = app.Arg("outdir", "Output dir").Required().String()
	profileArg          = app.Flag("profile", "Profile to build").String()
	resumeArg           = app.Flag("resume", "Which package to resume the build at").String()
	concurrencyLevelArg = app.Flag("concurrency", "The N in -jN to pass to make").Short('j').Default("2").String()
)

func main() {
	makeConcurrencyFlag := "-j" + (*concurrencyLevelArg)

	_, err := app.Parse(os.Args[1:])
	if err != nil {
		ctx, _ := app.ParseContext(os.Args[1:])
		app.FatalUsageContext(ctx, "%s\n", err.Error())
	}

	configBytes, err := ioutil.ReadFile(*configPath)
	if err != nil {
		log.Fatal("While reading config", err)
	}

	var config Config
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		log.Fatal("While parsing config", err)
	}

	outDir, err := filepath.Abs(*outDirArg)
	if err != nil {
		log.Fatal("While absolutizing outDir", err)
	}

	log.Printf("Config: %#v", config)
	for _, profile := range config.Profiles {
		if *profileArg != "" && *profileArg != profile.Name {
			log.Println("Skipping", profile.Name)
			continue
		}

		log.Println("Dealing with profile", profile.Name)

		src := filepath.Join(outDir, "src", profile.Name)
		prefix := filepath.Join(outDir, profile.Name)

		err = os.MkdirAll(src, 0755)
		if err != nil {
			log.Fatal("While creating source directory", err)
		}

		err = os.MkdirAll(prefix, 0755)
		if err != nil {
			log.Fatal("While creating prefix directory", err)
		}

		skipping := false
		if *resumeArg != "" {
			skipping = true
		}

		for _, pkg := range config.Packages {
			if pkg.Name == *resumeArg {
				skipping = false
			}

			if skipping {
				log.Println("Skipping", pkg.Name)
				continue
			}

			log.Println("Preparing", pkg.Name)
			env := []string{}
			for k, v := range profile.Env {
				env = append(env, fmt.Sprintf("%s=%s", k, v))
			}
			env = append(env, fmt.Sprintf("PREFIX=%s", prefix))

			pkgSrc := filepath.Join(src, pkg.Name)
			err = os.MkdirAll(pkgSrc, 0755)
			if err != nil {
				log.Fatal("While package source directory", err)
			}

			log.Println("Downloading from", pkg.Sources)

			format := pkg.Format
			if format == "" {
				if strings.Contains(pkg.Sources, ".tar.xz") {
					format = "tar.xz"
				} else if strings.Contains(pkg.Sources, ".tar.gz") {
					format = "tar.gz"
				} else {
					log.Fatal("Could not figure out format of", pkg.Sources, "please specify explicitly")
				}
			}

			pkgArchive := filepath.Join(pkgSrc, fmt.Sprintf("%s.%s", pkg.Name, format))
			pkgWriter, err := os.Create(pkgArchive)
			if err != nil {
				log.Fatal(err)
			}

			res, err := http.Get(pkg.Sources)
			if err != nil {
				log.Fatal(err)
			}
			defer res.Body.Close()

			if res.StatusCode != 200 {
				log.Fatal("HTTP", res.StatusCode, "for", pkg.Sources)
			}

			humanSize := "? bytes"
			if res.ContentLength > 0 {
				humanSize = humanize.IBytes(uint64(res.ContentLength))
			}
			log.Println("Downloading", humanSize)

			_, err = io.Copy(pkgWriter, res.Body)
			if err != nil {
				log.Fatal("While downloading", err)
			}

			err = pkgWriter.Close()
			if err != nil {
				log.Fatal(err)
			}

			log.Printf("Extracting...")
			tarFlags, err := tarFlagsForFormat(format)
			if err != nil {
				log.Fatal(err)
			}

			err = command("tar", env, tarFlags, pkgArchive, "-C", pkgSrc)
			if err != nil {
				log.Fatal(err)
			}

			files, err := ioutil.ReadDir(pkgSrc)
			if err != nil {
				log.Fatal(err)
			}

			var dir os.FileInfo
			for _, f := range files {
				if f.IsDir() {
					dir = f
					break
				}
			}

			baseWd, err := os.Getwd()
			if err != nil {
				log.Fatal(err)
			}

			srcDir := filepath.Join(pkgSrc, dir.Name())

			func() {
				log.Println("Entering", srcDir)
				err = os.Chdir(srcDir)
				if err != nil {
					log.Fatal(err)
				}
				defer os.Chdir(baseWd)

				configureArgs := []string{}
				configureArgs = append(configureArgs, "--prefix="+prefix)

				configureBlacklist := &Blacklist{Prefixes: pkg.ConfigureBlacklist}

				for _, arg := range profile.Configure {
					if !configureBlacklist.Has(arg) {
						configureArgs = append(configureArgs, arg)
					}
				}

				for _, arg := range pkg.Configure {
					if !configureBlacklist.Has(arg) {
						configureArgs = append(configureArgs, arg)
					}
				}

				log.Println("Configuring...")

				err = command("./configure", env, configureArgs...)
				if err != nil {
					log.Fatal(err)
				}

				log.Println("Building...")

				err = command("make", env, makeConcurrencyFlag)
				if err != nil {
					log.Fatal(err)
				}

				log.Println("Installing...")

				err = command("make", env, "install")
				if err != nil {
					log.Fatal(err)
				}
			}()
		}
	}

	log.Println("All done!")
}

func tarFlagsForFormat(format string) (string, error) {
	switch format {
	case "tar.gz":
		return "xf", nil
	case "tar.xz":
		return "xf", nil
	default:
		return "", fmt.Errorf("tarFlags: unknown format %s", format)
	}
}

func command(exe string, env []string, args ...string) error {
	log.Printf("> %s %s", exe, strings.Join(args, " "))
	log.Printf("> env: %s", strings.Join(env, " "))

	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	return cmd.Run()
}
