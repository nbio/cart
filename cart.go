package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	buildListURL = "https://circleci.com/api/v1/project/%s/tree/%s?limit=1&filter=successful&circle-token=%s"
	artifactsURL = "https://circleci.com/api/v1/project/%s/%d/artifacts?circle-token=%s"
)

type build struct {
	BuildNum int `json:"build_num"`
}

type artifact struct {
	URL string `json:"url"`
}

var (
	project         string
	branch          string
	buildNum        int
	circleToken     string
	outputPath      string
	verbose, dryRun bool
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("Error: %s", err)
	}
}

// Main allows deferred functions to fire before exiting.
func Main() error {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	flag.StringVar(&project, "repo", "", "github <username>/<repo>")
	flag.StringVar(&branch, "branch", "master", "search builds for branch")
	flag.IntVar(&buildNum, "build", 0, "get artifact for build #<n>, ignoring branch")
	flag.StringVar(&circleToken, "token", "", "CircleCI auth token")
	flag.StringVar(&outputPath, "o", "", "(optional) output file path")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&dryRun, "n", false, "skip artifact download")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <artifact>\n\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	flag.Parse()

	if project == "" {
		project = gitProject()
	}

	artifactName := flag.Arg(0)
	if circleToken == "" {
		circleToken = os.Getenv("CIRCLE_TOKEN")
	}

	switch {
	case project == "":
		flag.Usage()
		return fmt.Errorf("no <username>/<project> provided")
	case artifactName == "":
		flag.Usage()
		return fmt.Errorf("no <artifact> provided")
	case circleToken == "":
		flag.Usage()
		return fmt.Errorf("no auth token set: use $CIRCLE_TOKEN or flag -token")
	case buildNum > 0:
		// Don't look for a green build.
		log.Printf("Build: %d", buildNum)
	default:
		u := fmt.Sprintf(buildListURL, project, branch, circleToken)
		if verbose {
			log.Println("Build list:", u)
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var builds []build
		if err := json.NewDecoder(res.Body).Decode(&builds); err != nil {
			return err
		}
		if len(builds) == 0 {
			return fmt.Errorf("No builds found for branch: %s", branch)
		}
		buildNum = builds[0].BuildNum
		log.Printf("Build: %d branch: %s", buildNum, branch)
	}

	// Get artifact from buildNum
	u := fmt.Sprintf(artifactsURL, project, buildNum, circleToken)
	if verbose {
		log.Println("Artifact list:", u)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var artifacts []artifact
	if err := json.NewDecoder(res.Body).Decode(&artifacts); err != nil {
		return err
	}
	if outputPath == "" {
		outputPath = filepath.Base(artifactName)
	}
	n, err := downloadArtifact(artifacts, artifactName, outputPath)
	if err != nil {
		return err
	}
	log.Printf("Wrote %s (%d bytes) to %s", artifactName, n, outputPath)
	return nil
}

func downloadArtifact(artifacts []artifact, name, outputPath string) (int64, error) {
	for _, a := range artifacts {
		if verbose {
			log.Println("Artifact URL:", a.URL)
		}
		if !strings.HasSuffix(a.URL, name) {
			continue
		}
		u, err := url.Parse(a.URL)
		if err != nil {
			return 0, err
		}
		q := u.Query()
		q.Add("circle-token", circleToken)
		u.RawQuery = q.Encode()
		if verbose {
			log.Println("Artifact found:", name)
		}
		log.Printf("Downloading %s...", name)
		if dryRun {
			log.Println("dry run: skipped download")
			os.Exit(0)
		}
		res, err := http.Get(u.String())
		if err != nil {
			return 0, err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return 0, fmt.Errorf("http: remote server responded %s (check http://status.circleci.com)", res.Status)
		}
		f, err := os.Create(outputPath)
		if err != nil {
			return 0, err
		}
		return io.Copy(f, res.Body)
	}
	return 0, fmt.Errorf("Unable to find artifact: %s", name)
}

var ghURL = regexp.MustCompile(`github\.com/([^\s]+)`)

func gitProject() string {
	out, err := exec.Command("git", "remote", "-v").Output()
	if err != nil {
		return ""
	}
	s := bufio.NewScanner(bytes.NewBuffer(out))
	for s.Scan() {
		if bytes.Contains(s.Bytes(), []byte("origin")) {
			remote := ghURL.FindStringSubmatch(s.Text())
			if len(remote) > 1 {
				return strings.Replace(remote[1], ".git", "", 1)
			}
		}
	}
	return ""
}
