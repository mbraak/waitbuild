package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// circleCIBaseURL is the CircleCI API; a variable so tests can point it at a
// fake server.
var circleCIBaseURL = "https://circleci.com/api/v2"

// circleCIPipelinePages limits how many pages of the project's pipelines are
// searched for the commit. Pipelines are listed newest first, so a commit that
// was just pushed is on the first page.
const circleCIPipelinePages = 3

// circleCI asks CircleCI itself about the build of a commit. CircleCI reports
// to GitHub only once it has started jobs, so while a pipeline is waiting to
// be set up (which can take minutes when CircleCI is slow) GitHub knows
// nothing about it and the build would look finished. A nil *circleCI asks
// nothing.
type circleCI struct {
	token   string
	project string // project slug, "gh/owner/repo"
	branch  string // narrows the pipeline search; "" searches all branches
	missing bool   // CircleCI does not know the project; stop asking
}

// newCircleCI returns a client for the CircleCI project of owner/repo, or nil
// when no CircleCI token is configured. branch may be "" when the branch is
// not known.
func newCircleCI(owner, repo, branch string) *circleCI {
	token := circleCIToken()
	if token == "" {
		return nil
	}
	return &circleCI{token: token, project: "gh/" + owner + "/" + repo, branch: branch}
}

// circleCIToken returns the CircleCI API token from CIRCLE_TOKEN (the variable
// the CircleCI CLI reads) or CIRCLECI_TOKEN, falling back to the token line
// of a CircleCI CLI config file and then to the token the CircleCI CLI keeps
// in the macOS keychain. It returns "" when there is none.
func circleCIToken() string {
	for _, env := range []string{"CIRCLE_TOKEN", "CIRCLECI_TOKEN"} {
		if t := os.Getenv(env); t != "" {
			return t
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	config := filepath.Join(home, ".config", "circleci", "config.yml")
	for _, path := range []string{config, filepath.Join(home, ".circleci", "cli.yml")} {
		if t := yamlValue(path, "token"); t != "" {
			return t
		}
	}
	host := os.Getenv("CIRCLE_HOST")
	if host == "" {
		host = yamlValue(config, "host")
	}
	if host == "" {
		host = "https://circleci.com"
	}
	return keychainToken("com.circleci.cli:" + host)
}

// keychainToken returns the password of the keychain item with the given
// service, as stored by the CircleCI CLI through go-keyring, or "" when there
// is none. go-keyring may store the password base64 encoded behind a prefix.
func keychainToken(service string) string {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(string(out))
	if enc, ok := strings.CutPrefix(t, "go-keyring-base64:"); ok {
		dec, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return ""
		}
		t = string(dec)
	}
	return t
}

// yamlValue returns the value of a top-level scalar key in a simple YAML
// file, or "" when the file or key does not exist.
func yamlValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && k == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// circleCIPipeline and circleCIWorkflow are the parts of CircleCI's API
// responses that waitbuild uses.
type circleCIPipeline struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
	State  string `json:"state"`
	VCS    struct {
		Revision string `json:"revision"`
	} `json:"vcs"`
}

type circleCIWorkflow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// circleCIPage is one page of a CircleCI listing.
type circleCIPage[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token"`
}

// errCircleCINotFound is returned by get for HTTP 404.
var errCircleCINotFound = errors.New("not found")

// get fetches path (relative to the API) into v.
func (c *circleCI) get(ctx context.Context, path string, query url.Values, v any) error {
	u := circleCIBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Circle-Token", c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errCircleCINotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// checks returns the CircleCI build of sha as checks, one per workflow, and
// whether CircleCI has a pipeline for sha at all. A pipeline that CircleCI
// has not set up yet is reported as a single pending check, so that the build
// is not taken for finished while it waits.
func (c *circleCI) checks(ctx context.Context, sha string) ([]check, bool, error) {
	if c == nil || c.missing {
		return nil, false, nil
	}
	p, err := c.findPipeline(ctx, sha)
	if errors.Is(err, errCircleCINotFound) {
		c.missing = true
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("listing CircleCI pipelines: %w", err)
	}
	if p == nil {
		return nil, false, nil
	}

	pipelineURL := fmt.Sprintf("https://app.circleci.com/pipelines/%s/%d", c.appProject(), p.Number)
	switch p.State {
	case "created":
	case "errored":
		return []check{{key: "circleci:pipeline", name: "circleci: pipeline", status: "completed", conclusion: "error", url: pipelineURL}}, true, nil
	default: // setup-pending, setup, pending
		return []check{{key: "circleci:pipeline", name: "circleci: pipeline", status: "pending", url: pipelineURL}}, true, nil
	}

	workflows, err := c.workflows(ctx, p.ID)
	if err != nil {
		return nil, false, fmt.Errorf("listing CircleCI workflows: %w", err)
	}
	var all []check
	for _, w := range workflows {
		c := check{
			key:  "circleci:" + w.Name,
			name: "circleci: " + w.Name,
			url:  pipelineURL + "/workflows/" + w.ID,
		}
		c.status, c.conclusion = circleCIWorkflowState(w.Status)
		all = append(all, c)
	}
	return all, true, nil
}

// appProject returns the project as it appears in CircleCI web app URLs,
// which spell out "github" where the API's project slug says "gh".
func (c *circleCI) appProject() string {
	if rest, ok := strings.CutPrefix(c.project, "gh/"); ok {
		return "github/" + rest
	}
	return c.project
}

// findPipeline returns the newest pipeline of the project that built sha, or
// nil when there is none among the most recent pipelines.
func (c *circleCI) findPipeline(ctx context.Context, sha string) (*circleCIPipeline, error) {
	query := url.Values{}
	if c.branch != "" {
		query.Set("branch", c.branch)
	}
	for range circleCIPipelinePages {
		var page circleCIPage[circleCIPipeline]
		if err := c.get(ctx, "/project/"+c.project+"/pipeline", query, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Items {
			if p.VCS.Revision == sha {
				return &p, nil
			}
		}
		if page.NextPageToken == "" {
			break
		}
		query.Set("page-token", page.NextPageToken)
	}
	return nil, nil
}

// workflows returns the workflows of a pipeline. A rerun adds a new workflow
// with the same name to the pipeline; only the newest of each name is
// returned.
func (c *circleCI) workflows(ctx context.Context, pipelineID string) ([]circleCIWorkflow, error) {
	newest := map[string]circleCIWorkflow{}
	var order []string
	query := url.Values{}
	for {
		var page circleCIPage[circleCIWorkflow]
		if err := c.get(ctx, "/pipeline/"+pipelineID+"/workflow", query, &page); err != nil {
			return nil, err
		}
		for _, w := range page.Items {
			prev, seen := newest[w.Name]
			if !seen {
				order = append(order, w.Name)
			}
			// RFC 3339 timestamps in UTC compare correctly as strings.
			if !seen || w.CreatedAt > prev.CreatedAt {
				newest[w.Name] = w
			}
		}
		if page.NextPageToken == "" {
			break
		}
		query.Set("page-token", page.NextPageToken)
	}
	out := make([]circleCIWorkflow, len(order))
	for i, name := range order {
		out[i] = newest[name]
	}
	return out, nil
}

// circleCIWorkflowState maps the status of a CircleCI workflow to the status
// and conclusion of a check. "failing" means a job failed while others still
// run. A workflow on hold waits for a manual approval, usually to deploy; the
// build itself is done, so it counts as completed.
func circleCIWorkflowState(status string) (string, string) {
	switch status {
	case "running", "failing":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "on_hold":
		return "completed", "on_hold"
	case "not_run":
		return "completed", "skipped"
	case "canceled":
		return "completed", "cancelled"
	case "failed":
		return "completed", "failure"
	case "error", "unauthorized":
		return "completed", "error"
	}
	return status, "" // unknown: keep waiting
}

// fromCircleCI reports whether c is a status or check run that CircleCI
// posted to GitHub. When waitbuild asks CircleCI directly, these duplicate
// its workflows.
func (c check) fromCircleCI() bool {
	return strings.HasPrefix(c.key, "status:ci/circleci:") || strings.HasPrefix(c.app, "circleci")
}
