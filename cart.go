package main

import (
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
	BuildNum int    `json:"build_num"`
	Revision string `json:"vcs_revision"`
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

	flag.StringVar(&project, "repo", "", "github `username/repo`")
	flag.StringVar(&branch, "branch", "master", "search builds for branch `name`")
	flag.IntVar(&buildNum, "build", 0, "get artifact for build number, ignoring branch")
	flag.StringVar(&circleToken, "token", "", "CircleCI auth token")
	flag.StringVar(&outputPath, "o", "", "output file `path`")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&dryRun, "n", false, "skip artifact download")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <artifact>\n\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	flag.Parse()

	if project == "" {
		out, err := exec.Command("git", "remote", "get-url", "origin").Output()
		if err != nil {
			return fmt.Errorf("exec git: %s", err)
		}
		project = gitProject(string(out))
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
			return fmt.Errorf("no builds found for branch: %s", branch)
		}
		build := builds[0]
		buildNum = build.BuildNum
		log.Printf("build: %d branch: %s rev: %s", buildNum, branch, build.Revision[:8])
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
		if dryRun {
			log.Println("Dry run: skipped download")
			os.Exit(0)
		}
		log.Printf("Downloading %s...", name)
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
	return 0, fmt.Errorf("unable to find artifact: %s", name)
}

var ghURL = regexp.MustCompile(`github\.com(?:/|:)(\w+/\w+)`)

func gitProject(url string) string {
	remote := ghURL.FindStringSubmatch(url)
	if len(remote) > 1 {
		return strings.Replace(remote[1], ".git", "", 1)
	}
	return ""
}
