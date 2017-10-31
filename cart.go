package main

import (
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
	// The limit in this URL needs to be at least as long as the number of builds in a workflow
	buildListURL = "https://circleci.com/api/v1/project/%s/tree/%s?limit=%d&filter=successful&circle-token=%s"
	artifactsURL = "https://circleci.com/api/v1/project/%s/%d/artifacts?circle-token=%s"
)

type workflow struct {
	JobName      string `json:"job_name"`
	JobId        string `json:"job_id"`
	WorkflowName string `json:"workflow_name"`
	WorkflowId   string `json:"workflow_id"`
}

type build struct {
	BuildNum  int       `json:"build_num"`
	Revision  string    `json:"vcs_revision"`
	Workflows *workflow `json:"workflows"` // plural name but singleton struct
}

type artifact struct {
	URL string `json:"url"`
}

var (
	project               string
	branch                string
	buildNum              int
	circleToken           string
	outputPath            string
	workflowArtifactBuild string
	workflowDepth         int
	verbose, dryRun       bool
)

func main() {
	log.SetFlags(log.Lshortfile)
	log.SetOutput(os.Stderr)

	flag.StringVar(&project, "repo", "", "github `username/repo`")
	flag.StringVar(&branch, "branch", "master", "search builds for branch `name`")
	flag.IntVar(&buildNum, "build", 0, "get artifact for build number, ignoring branch")
	flag.StringVar(&workflowArtifactBuild, "workflow-artifact-build", "",
		"if branch build was workflow, look for this name build to find the artifacts")
	flag.IntVar(&workflowDepth, "workflow-depth", 2, "how far back to look for workflows")
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
			log.Fatalf("exec git: %s", err)
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
		log.Fatal("no <username>/<project> provided")
	case artifactName == "":
		flag.Usage()
		log.Fatal("no <artifact> provided")
	case circleToken == "":
		flag.Usage()
		log.Fatal("no auth token set: use $CIRCLE_TOKEN or flag -token")
	case workflowDepth < 1:
		flag.Usage()
		log.Fatal("workflow depth must be a positive (smallish) integer")
	case buildNum > 0:
		// Don't look for a green build.
		fmt.Printf("Build: %d\n", buildNum)
	default:
		buildNum = circleFindBuild(project, branch, circleToken)
	}

	// Get artifact from buildNum
	u := fmt.Sprintf(artifactsURL, project, buildNum, circleToken)
	if verbose {
		fmt.Println("Artifact list:", u)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	var artifacts []artifact
	if err := json.NewDecoder(res.Body).Decode(&artifacts); err != nil {
		log.Fatal(err)
	}
	if outputPath == "" {
		outputPath = filepath.Base(artifactName)
	}
	n, err := downloadArtifact(artifacts, artifactName, outputPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote %s (%d bytes) to %s\n", artifactName, n, outputPath)
}

func circleFindBuild(project, branch, circleToken string) (buildNum int) {
	u := fmt.Sprintf(buildListURL, project, branch, workflowDepth, circleToken)
	if verbose {
		fmt.Println("Build list:", u)
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	body := new(bytes.Buffer)
	if _, err := io.Copy(body, res.Body); err != nil {
		log.Fatal(err)
	}
	var (
		builds []build
		build  build
	)
	if err := json.Unmarshal(body.Bytes(), &builds); err != nil {
		log.Fatalf("%s: %s", err, body.String())
	}
	if len(builds) == 0 {
		log.Fatalf("no builds found for branch: %s", branch)
	}
	if workflowArtifactBuild != "" && builds[0].Workflows != nil && builds[0].Workflows.JobName != workflowArtifactBuild {
		fmt.Printf("build: branch %q build %d is %q part of workflow %q, searching for build %q\n",
			branch, builds[0].BuildNum, builds[0].Workflows.JobName,
			builds[0].Workflows.WorkflowName, workflowArtifactBuild)
		if len(builds) < 2 {
			log.Fatalf("build: failed to find a build %q in workflow %q branch %q",
				workflowArtifactBuild, builds[0].Workflows.WorkflowName, branch)
		}
		for i := 1; i < len(builds); i++ {
			if builds[i].Workflows != nil &&
				builds[i].Workflows.WorkflowId == builds[0].Workflows.WorkflowId &&
				builds[i].Workflows.JobName == workflowArtifactBuild {
				fmt.Printf("build: workflow %q branch %q found build %q at offset %d\n",
					builds[0].Workflows.WorkflowName, branch, workflowArtifactBuild, i)
				build = builds[i]
				buildNum = build.BuildNum
				break
			}
		}
		if buildNum == 0 {
			log.Fatalf("build: failed to find a suitable build for workflow %q (%q)",
				builds[0].Workflows.WorkflowName, builds[0].Workflows.WorkflowId)
		}
	} else {
		build = builds[0]
		buildNum = build.BuildNum
	}
	fmt.Printf("build: %d branch: %s rev: %s\n", buildNum, branch, build.Revision[:8])
	return
}

func downloadArtifact(artifacts []artifact, name, outputPath string) (int64, error) {
	for _, a := range artifacts {
		if verbose {
			fmt.Println("Artifact URL:", a.URL)
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
			fmt.Println("Artifact found:", name)
		}
		if dryRun {
			fmt.Println("Dry run: skipped download")
			os.Exit(0)
		}
		fmt.Printf("Downloading %s...\n", name)
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
