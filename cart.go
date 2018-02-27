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
	"strconv"
	"strings"
)

const (
	// API v1.1 : <https://circleci.com/docs/api/v1-reference/>
	// but beware that the summary is missing some method/URL pairs which are
	// described further down in the page.

	buildListURL = "https://circleci.com/api/v1.1/project/github/${project}/tree/${branch}?limit=${retrieve_count}&filter=successful&circle-token=${circle_token}"
	artifactsURL = "https://circleci.com/api/v1.1/project/github/${project}/${build_num}/artifacts?circle-token=${circle_token}"

	// We need to account for multiple workflows, and multiple builds within workflows
	defaultRetrieveCount = 10
)

type workflow struct {
	JobName      string `json:"job_name"`
	JobID        string `json:"job_id"`
	WorkflowName string `json:"workflow_name"`
	WorkflowID   string `json:"workflow_id"`
}

type build struct {
	BuildNum  int       `json:"build_num"`
	Revision  string    `json:"vcs_revision"`
	Workflows *workflow `json:"workflows"` // plural name but singleton struct

	// We want to skip bad builds, and perhaps print the others so that if
	// there's a mismatch from expectations, folks might notice.
	Status   string `json:"status"`
	Subject  string `json:"subject"`
	StopTime string `json:"stop_time"`
}

type artifact struct {
	URL string `json:"url"`
}

// FilterSet is the collection of attributes upon which we filter the results
// from Circle CI (or provide in URL to pre-filter).
type FilterSet struct {
	branch    string
	workflow  string
	jobname   string
	anyFlowID bool
}

// Expander is used to take strings containing ${var} and interpolate them,
// so that we don't have URLs which have %s/%s/%s and cross-referencing across
// places to figure out which those fields are.
type Expander map[string]string

// Get is just a map lookup which panics, as a function for use with os.Expand
func (e Expander) Get(key string) string {
	if val, ok := e[key]; ok {
		return val
	}
	// There is no recovery, we don't want to pass a bad URL out, we're
	// a client tool and we'll need to fix the hardcoded template strings.
	panic("bad key " + key)
}

// Expand converts "${foo}/${bar}" into "football/goal".
// It also handles some $foo without parens, but we avoid using that.
func (e *Expander) Expand(src string) string {
	return os.Expand(src, e.Get)
}

var (
	circleToken     string
	filter          FilterSet
	verbose, dryRun bool
)

func main() {
	var (
		project             string
		buildNum            int
		outputPath          string
		retrieveBuildsCount int
	)

	log.SetFlags(log.Lshortfile)
	log.SetOutput(os.Stderr)

	flag.StringVar(&circleToken, "token", "", "CircleCI auth token")
	flag.StringVar(&outputPath, "o", "", "output file `path`")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&dryRun, "n", false, "skip artifact download")

	flag.StringVar(&project, "repo", "", "github `username/repo`")
	flag.IntVar(&buildNum, "build", 0, "get artifact for build number, ignoring branch")
	flag.StringVar(&filter.branch, "branch", "master", "search builds for branch `name`")

	// Workflows:
	// If there are multiple workflows, then the latest "build" is perhaps unrelated to building,
	// not even a later step in a workflow where an earlier step did build.  Eg, we have
	// stuff to automate dependencies checking, scheduled from cron.
	// So to retrieve an artifact, we want to only consider specific workflow names.
	// HOWEVER: those are config items in `.circleci/config.yml` and we should avoid hardcoding
	// such arbitrary choices across more than one repo, so our default for now is empty,
	// thus not filtered.
	//
	// Within a workflow, the build might not be the last step in the flow; it usually won't be.
	// Later steps might be "deploy", "stash image somewhere", etc.
	// So we need to step back from the last step within a workflow until we find the specific
	// step we're told.
	//
	// Eg, for one project, at this time, we use "commit_workflow" as the workflow to search for
	// and "build" as the job within that workflow.

	flag.StringVar(&filter.workflow, "workflow", "", "only consider builds which are part of this workflow")
	flag.StringVar(&filter.jobname, "jobname", "", "look within workflow for artifacts from this job's name")
	flag.IntVar(&retrieveBuildsCount, "retrieve-count", defaultRetrieveCount, "how many builds to retrieve")
	// This description is too long; how to make it shorter?
	flag.BoolVar(&filter.anyFlowID, "ignore-later-workflows", false, "get the last successful workflow/job build, even if of a previous flow-id than the latest we see for that workflow")

	// when the workflow-jobname functionality was first added, I (pdp) named it badly; for compatibility,
	// continue taking the confusingly named option, but map it to the fixed variable.  Similarly for
	// how the presence of multiple workflows means "workflow-depth" was now a misnomer, and "retrieve-count"
	// is more accurate.
	flag.StringVar(&filter.jobname, "workflow-artifact-build", "", "(alias for -workflow-jobname)")
	flag.IntVar(&retrieveBuildsCount, "workflow-depth", defaultRetrieveCount, "(alias for -retrieve-count)")

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

	// for URL expansion with sane named parameters, and put in everything
	// we might want too, including filters, in case there are better
	// URLs we can switch to in future.
	expansions := Expander{
		"project":        project,
		"artifact":       artifactName,
		"retrieve_count": strconv.Itoa(retrieveBuildsCount),
		"build_num":      strconv.Itoa(buildNum),
		"circle_token":   circleToken,
		"branch":         filter.branch,
		"workflow":       filter.workflow,
		"jobname":        filter.jobname,
	}

	switch {
	case project == "":
		flag.Usage()
		log.Fatal("no <username>/<project> provided")
	case filter.branch == "":
		flag.Usage()
		log.Fatal("no <branch> provided")
	case artifactName == "":
		flag.Usage()
		log.Fatal("no <artifact> provided")
	case circleToken == "":
		flag.Usage()
		log.Fatal("no auth token set: use $CIRCLE_TOKEN or flag -token")
	case retrieveBuildsCount < 1:
		flag.Usage()
		log.Fatal("workflow depth must be a positive (smallish) integer")
	case buildNum > 0:
		// Don't look for a green build.
		fmt.Printf("Build: %d\n", buildNum)
	default:
		buildNum = circleFindBuild(expansions, filter)
		expansions["build_num"] = strconv.Itoa(buildNum)
	}

	// Get artifact from buildNum
	u := expansions.Expand(artifactsURL)
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

func circleFindBuild(expansions Expander, filter FilterSet) (buildNum int) {
	u := expansions.Expand(buildListURL)
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

	var builds []build
	if err := json.Unmarshal(body.Bytes(), &builds); err != nil {
		log.Fatalf("%s: %s", err, body.String())
	}
	if len(builds) == 0 {
		log.Fatalf("no builds found for branch: %s", filter.branch)
	}

	// We _want_ to find the last successful workflow; as of APIv1.1 there's
	// nothing to filter directly by workflow, nor to tell if a workflow has
	// completed successfully, to know if we're grabbing something which later
	// failed, etc.
	//
	// So we just look for the last green build within a workflow and rely upon
	// the build we want being either that one, or earlier, with no prep steps
	// pre-build.  Unless the caller told us they don't care about matching
	// workflow ID to the latest workflow for which we see any builds.

	foundBuild := -1
	onlyWorkflowID := ""
	for i := 0; i < len(builds); i++ {
		headOfWorkflow := false
		if builds[i].Workflows == nil && (filter.workflow != "" || filter.jobname != "") {
			// fmt.Printf("skipping %d, no workflow: %+v\n", i, builds[i])
			// -- these happen, they show in the UI, I wonder if it's a manual trigger?
			continue
		}
		if builds[i].Status != "success" {
			continue
		}
		if onlyWorkflowID != "" && builds[i].Workflows.WorkflowID != onlyWorkflowID {
			continue
		}
		if filter.workflow != "" && builds[i].Workflows.WorkflowName != filter.workflow {
			continue
		}
		if onlyWorkflowID == "" && filter.workflow != "" && !filter.anyFlowID {
			onlyWorkflowID = builds[i].Workflows.WorkflowID
			headOfWorkflow = true
		}
		if filter.jobname != "" && builds[i].Workflows.JobName != filter.jobname {
			if headOfWorkflow {
				fmt.Printf("build: branch %q build %d is a %q, part of workflow %q, searching for build %q\n",
					filter.branch, builds[i].BuildNum,
					builds[i].Workflows.JobName, builds[0].Workflows.WorkflowName,
					filter.jobname)
			}
			continue
		}
		if builds[i].Workflows == nil {
			// must mean no filters, so i == 0
			fmt.Printf("build: workflow-less on branch %q found a build at offset %d\n",
				filter.branch, i)
		} else {
			fmt.Printf("build: workflow %q branch %q found build %q at offset %d\n",
				builds[i].Workflows.WorkflowName, filter.branch, builds[i].Workflows.JobName, i)
		}

		foundBuild = i
		break
	}

	if foundBuild < 0 {
		labelFlow := filter.workflow
		labelName := filter.jobname
		if labelFlow == "" {
			labelFlow = "*"
		}
		if labelName == "" {
			labelName = "*"
		}
		log.Fatalf("build: failed to find a build matching workflow=%q jobname=%q in branch %q",
			labelFlow, labelName, filter.branch)
	}

	if verbose {
		fmt.Printf("\nBuild Subject  : %s\nBuild Finished : %s\n",
			builds[foundBuild].Subject, builds[foundBuild].StopTime)
	}

	fmt.Printf("build: %d branch: %s rev: %s\n",
		builds[foundBuild].BuildNum, filter.branch, builds[foundBuild].Revision[:8])
	return builds[foundBuild].BuildNum
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
