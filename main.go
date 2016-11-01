package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"

	"github.com/appc/spec/schema"
)

type CommandType int

const (
	Compile CommandType = iota
	Run
	Start
	Stop
	Status
	Logs
)

var (
	templatePath    string
	basePath        string
	podName         string
	slice           string
	outfile         string
	command         CommandType
	additionalFlags []string
)

func init() {
	flagSet := flag.NewFlagSet("flags", flag.ExitOnError)
	flagSet.StringVar(&templatePath, "template", "pod-template.yaml", "pod-template to use")
	flagSet.StringVar(&templatePath, "t", "pod-template.yaml", "pod-template to use")
	flagSet.StringVar(&basePath, "base", "./", "basepath to prepend all relative volume pathes")
	flagSet.StringVar(&basePath, "b", "./", "basepath to prepend all relative volume pathes")
	flagSet.StringVar(&podName, "name", "", "name of the pod")
	flagSet.StringVar(&podName, "n", "", "name of the pod")
	flagSet.StringVar(&outfile, "out", "/dev/stdout", "name of the pod manifest to write")
	flagSet.StringVar(&outfile, "o", "/dev/stdout", "name of the pod manifest to write")
	flagSet.StringVar(&slice, "slice", "", "slice of the pod")
	flagSet.StringVar(&slice, "s", "", "slice of the pod")
	args := os.Args
	if len(args) == 1 {
		log.Fatal("specify one of compile, run, start, stop, logs or status")
	}
	switch args[1] {
	case "-h":
		fallthrough
	case "--help":
		fmt.Printf("usage: %v <compile|run|start|stop|logs|status> [args]\n", os.Args[0])
		flagSet.PrintDefaults()
		os.Exit(0)
	case "compile":
		command = Compile
	case "run":
		command = Run
	case "start":
		command = Start
	case "stop":
		command = Stop
	case "status":
		command = Status
	case "logs":
		command = Logs
	default:
		{
			log.Fatal("specify one of compile, run, start, stop, logs or status")
		}
	}
	for idx, val := range args {
		if val == "--" {
			additionalFlags = args[idx+1:]
			args = args[:idx]
			break
		}
	}
	err := flagSet.Parse(args[2:])
	if err != nil {
		log.Fatal(err)
	}
	if command == Start || command == Stop || command == Status || command == Logs {
		if podName == "" {
			log.Fatal("you must specify --name when working with pods in background")
		}
	}
}

func readTemplate() (*schema.PodManifest, error) {
	f, err := os.Open(templatePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bs, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	podTemplate := schema.BlankPodManifest()
	err = yaml.Unmarshal(bs, &podTemplate)
	if err != nil {
		return nil, err
	}
	return podTemplate, nil
}

func fetchImages(manifest *schema.PodManifest) error {
	for id, app := range manifest.Apps {
		if app.Image.ID.Empty() {
			var (
				name    = app.Image.Name.String()
				schema  string
				version string
			)
			for _, label := range app.Image.Labels {
				if label.Name.String() == "schema" {
					schema = label.Value
				} else if label.Name.String() == "version" {
					version = label.Value
				}
			}
			pullName := schema + name + ":" + version
			log.Printf("%v: No Image ID specified, fetching %v...", app.Name, pullName)
			args := []string{"rkt", "fetch", pullName}
			if schema == "docker://" {
				args = append(args, "--insecure-options=image")
			}
			cmd := exec.Command("sudo", args...)
			output := &bytes.Buffer{}
			cmd.Stdout = output
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil {
				return err
			}
			js := []byte{'"'}
			js = append(js, output.Bytes()...)
			js[len(js)-1] = '"'
			err = app.Image.ID.UnmarshalJSON(js)
			if err != nil {
				return err
			}
			manifest.Apps[id] = app
		}
	}
	return nil
}

func makeVolumeSourcesAbsolute(manifest *schema.PodManifest) error {
	for id, volume := range manifest.Volumes {
		if !filepath.IsAbs(volume.Source) {
			path, err := filepath.Abs(filepath.Join(basePath, volume.Source))
			if err != nil {
				return err
			}
			manifest.Volumes[id].Source = path
		}
	}
	return nil
}

func makeUserAndGroupValid(manifest *schema.PodManifest) error {
	for id, app := range manifest.Apps {
		if app.App.User == "" {
			manifest.Apps[id].App.User = "0"
		}
		if app.App.Group == "" {
			manifest.Apps[id].App.Group = "0"
		}
	}
	return nil
}

func runForeground(manifest *schema.PodManifest) error {
	buff := &bytes.Buffer{}
	encoder := json.NewEncoder(buff)
	err := encoder.Encode(manifest)
	if err != nil {
		return err
	}
	args := []string{"rkt", "run", "--pod-manifest=/dev/stdin"}
	args = append(args, additionalFlags...)
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = buff
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func writeManifest(manifest *schema.PodManifest) error {
	f, err := os.Create(outfile)
	if err != nil {
		return err
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}

func prepareManifest() (*schema.PodManifest, error) {
	manifest, err := readTemplate()
	if err != nil {
		return nil, err
	}
	err = fetchImages(manifest)
	if err != nil {
		return nil, err
	}
	err = makeVolumeSourcesAbsolute(manifest)
	if err != nil {
		return nil, err
	}
	makeUserAndGroupValid(manifest)
	return manifest, nil
}

func start(manifest *schema.PodManifest) error {
	stop()
	f, err := ioutil.TempFile("/tmp", "pod-manifest")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	err = encoder.Encode(manifest)
	if err != nil {
		return err
	}
	out := &bytes.Buffer{}
	args := []string{"systemd-run", "--unit", podName, "--slice", slice, "rkt", "run", "--pod-manifest=" + f.Name()}
	args = append(args, additionalFlags...)
	cmd := exec.Command("sudo", args...)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func stop() error {
	script := fmt.Sprintf("systemctl stop %v.service; systemctl reset-failed %v.service;", podName, podName)
	cmd := exec.Command("sudo", "bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func status() error {
	script := fmt.Sprintf("systemctl status %v.service;", podName)
	cmd := exec.Command("sudo", "bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func logs() error {
	script := fmt.Sprintf("journalctl -M $(systemctl status %v --no-pager|grep -Eo \"rkt-[a-f0-9-]*\") %v", podName, strings.Join(additionalFlags, " "))
	cmd := exec.Command("sudo", "bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	switch command {
	case Compile:
		{
			manifest, err := prepareManifest()
			if err != nil {
				log.Fatal(err)
			}
			err = writeManifest(manifest)
			if err != nil {
				log.Fatal(err)
			}
		}
	case Run:
		{
			manifest, err := prepareManifest()
			if err != nil {
				log.Fatal(err)
			}

			if err = runForeground(manifest); err != nil {
				log.Fatal(err)
			}
		}
	case Start:
		{
			manifest, err := prepareManifest()
			if err != nil {
				log.Fatal(err)
			}

			if err = start(manifest); err != nil {
				log.Fatal(err)
			}
		}
	case Stop:
		{
			if err := stop(); err != nil {
				log.Fatal(err)
			}
		}
	case Status:
		{
			if err := status(); err != nil {
				log.Fatal(err)
			}
		}
	case Logs:
		{
			if err := logs(); err != nil {
				log.Fatal(err)
			}
		}
	}
}
