// Package azdo implements Azure DevOps pull-request operations. With
// AZURE_DEVOPS_EXT_PAT set, REST calls go straight to the API with that PAT.
// Otherwise they go through `az devops invoke` (azure-devops extension), which
// mirrors how internal/gitlab shells out to glab api.
package azdo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	apiVersion = "7.1"
	// Same variable the azure-devops az extension reads, so one PAT serves both.
	patEnv = "AZURE_DEVOPS_EXT_PAT"
)

var resourcePaths = map[string]string{
	"pullRequests":              "pullRequests/{pullRequestId}",
	"items":                     "items",
	"pullRequestThreads":        "pullRequests/{pullRequestId}/threads/{threadId}",
	"pullRequestThreadComments": "pullRequests/{pullRequestId}/threads/{threadId}/comments/{commentId}",
}

var voteValues = map[string]int{"approve": 10, "wait-for-author": -5}

var commandContext = exec.CommandContext

// coords identifies one Azure DevOps Git repository.
type coords struct {
	OrgURL  string // https://dev.azure.com/<org> or https://<org>.visualstudio.com
	Project string
	Repo    string
}

// parseRepoURL extracts org, project, and repository from an HTTPS remote,
// an SSH remote, or a pull-request URL. It returns the path segments that
// follow the repository name so callers can read /pullrequest/<id>.
func parseRepoURL(raw string) (coords, []string, error) {
	raw = strings.TrimSpace(raw)
	invalid := fmt.Errorf("not an Azure DevOps repository URL: %q", raw)
	for _, marker := range []string{"ssh.dev.azure.com:v3/", "vs-ssh.visualstudio.com:v3/"} {
		i := strings.Index(raw, marker)
		if i < 0 {
			continue
		}
		segments, err := unescapeSegments(strings.Split(strings.Trim(raw[i+len(marker):], "/"), "/"))
		// An insteadOf rewrite of an HTTPS remote keeps the "_git" segment: v3/<org>/<project>/_git/<repo>.
		if len(segments) == 4 && segments[2] == "_git" {
			segments = append(segments[:2], segments[3])
		}
		if err != nil || len(segments) != 3 {
			return coords{}, nil, invalid
		}
		org := segments[0]
		orgURL := "https://dev.azure.com/" + org
		if strings.HasPrefix(marker, "vs-ssh") {
			orgURL = "https://" + org + ".visualstudio.com"
		}
		return coords{OrgURL: orgURL, Project: segments[1], Repo: strings.TrimSuffix(segments[2], ".git")}, nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return coords{}, nil, invalid
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	gitIndex := -1
	for i, segment := range segments {
		if segment == "_git" {
			gitIndex = i
			break
		}
	}
	if gitIndex < 1 || gitIndex+1 >= len(segments) {
		return coords{}, nil, invalid
	}
	result := coords{Project: segments[gitIndex-1], Repo: strings.TrimSuffix(segments[gitIndex+1], ".git")}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "dev.azure.com":
		result.OrgURL = "https://dev.azure.com/" + segments[0]
	case strings.HasSuffix(host, ".visualstudio.com"):
		result.OrgURL = "https://" + host
	default:
		return coords{}, nil, invalid
	}
	return result, segments[gitIndex+2:], nil
}

func unescapeSegments(segments []string) ([]string, error) {
	out := make([]string, len(segments))
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return nil, err
		}
		out[i] = decoded
	}
	return out, nil
}

// parsePRSpec accepts a PR number or an Azure DevOps pull-request URL. The
// returned coords are nil for a bare number (use the checkout remote).
func parsePRSpec(spec string) (int, *coords, error) {
	if n, err := strconv.Atoi(spec); err == nil && n > 0 {
		return n, nil, nil
	}
	invalid := fmt.Errorf("invalid pull request %q (expected number or https://dev.azure.com/<org>/<project>/_git/<repo>/pullrequest/<id> URL)", spec)
	repo, rest, err := parseRepoURL(spec)
	if err != nil || len(rest) < 2 || !strings.EqualFold(rest[0], "pullrequest") {
		return 0, nil, invalid
	}
	n, err := strconv.Atoi(rest[1])
	if err != nil || n <= 0 {
		return 0, nil, invalid
	}
	return n, &repo, nil
}

func originRemote(ctx context.Context) (string, error) {
	out, err := commandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("reading git remote origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func usePAT() bool { return os.Getenv(patEnv) != "" }

func requireAuth(ctx context.Context) error {
	if usePAT() {
		return nil
	}
	if _, err := exec.LookPath("az"); err != nil {
		return fmt.Errorf("az CLI not found. Set %s to a PAT with Code scope, or install az: https://learn.microsoft.com/cli/azure/install-azure-cli", patEnv)
	}
	if out, err := commandContext(ctx, "az", "extension", "show", "--name", "azure-devops", "--output", "none").CombinedOutput(); err != nil {
		return fmt.Errorf("az azure-devops extension is missing. Run: az extension add --name azure-devops: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func runAZ(ctx context.Context, args ...string) ([]byte, error) {
	cmd := commandContext(ctx, "az", append(args, "--output", "json")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("az %s: %s: %w", strings.Join(args[:min(3, len(args))], " "), strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}

// invoke calls one Git REST resource. route holds extra key=value route
// parameters, query holds key=value query parameters.
func (c coords) invoke(ctx context.Context, method, resource string, route, query []string, payload any) ([]byte, error) {
	if usePAT() {
		target, err := c.restURL(resource, route, query)
		if err != nil {
			return nil, err
		}
		return restCall(ctx, method, target, payload)
	}
	args := []string{"devops", "invoke", "--org", c.OrgURL, "--area", "git", "--resource", resource,
		"--http-method", method, "--api-version", apiVersion,
		"--route-parameters", "project=" + c.Project, "repositoryId=" + c.Repo}
	args = append(args, route...)
	if len(query) > 0 {
		args = append(append(args, "--query-parameters"), query...)
	}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal Azure DevOps payload: %w", err)
		}
		file, err := os.CreateTemp("", "crit-azdo-*.json")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		_, writeErr := file.Write(body)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return nil, fmt.Errorf("writing Azure DevOps payload: %w", err)
		}
		args = append(args, "--in-file", file.Name())
	}
	return runAZ(ctx, args...)
}

func (c coords) repoURL() string {
	return c.OrgURL + "/" + url.PathEscape(c.Project) + "/_apis/git/repositories/" + url.PathEscape(c.Repo)
}

// restURL builds the REST URL for an `az devops invoke` resource. A missing
// route parameter ends the path, so pullRequests without an id lists them.
func (c coords) restURL(resource string, route, query []string) (string, error) {
	template, ok := resourcePaths[resource]
	if !ok {
		return "", fmt.Errorf("unsupported Azure DevOps resource %q", resource)
	}
	params := keyValues(route)
	segments := []string{c.repoURL()}
	for _, segment := range strings.Split(template, "/") {
		if key, isParam := strings.CutPrefix(segment, "{"); isParam {
			value, ok := params[strings.TrimSuffix(key, "}")]
			if !ok {
				break
			}
			segment = url.PathEscape(value)
		}
		segments = append(segments, segment)
	}
	values := url.Values{"api-version": {apiVersion}}
	for key, value := range keyValues(query) {
		values.Set(key, value)
	}
	return strings.Join(segments, "/") + "?" + values.Encode(), nil
}

func keyValues(pairs []string) map[string]string {
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, _ := strings.Cut(pair, "=")
		out[key] = value
	}
	return out
}

func restCall(ctx context.Context, method, target string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal Azure DevOps payload: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("", os.Getenv(patEnv))
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Azure DevOps %s %s: %w", method, target, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading Azure DevOps %s %s: %w", method, target, err)
	}
	// ADO answers a rejected PAT with 203 and an HTML sign-in page, not 401.
	if resp.StatusCode >= 300 || resp.StatusCode == http.StatusNonAuthoritativeInfo {
		return nil, fmt.Errorf("Azure DevOps %s %s: %s: %s", method, target, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c coords) setVote(ctx context.Context, n int, vote string) error {
	if !usePAT() {
		_, err := runAZ(ctx, "repos", "pr", "set-vote", "--org", c.OrgURL, "--id", strconv.Itoa(n), "--vote", vote)
		return err
	}
	out, err := restCall(ctx, "GET", c.OrgURL+"/_apis/connectionData", nil)
	if err != nil {
		return err
	}
	var connection struct {
		AuthenticatedUser struct {
			ID string `json:"id"`
		} `json:"authenticatedUser"`
	}
	if err := json.Unmarshal(out, &connection); err != nil {
		return fmt.Errorf("parsing Azure DevOps connection data: %w", err)
	}
	target := fmt.Sprintf("%s/pullRequests/%d/reviewers/%s?api-version=%s", c.repoURL(), n, url.PathEscape(connection.AuthenticatedUser.ID), apiVersion)
	_, err = restCall(ctx, "PUT", target, map[string]int{"vote": voteValues[vote]})
	return err
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") || strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist")
}
