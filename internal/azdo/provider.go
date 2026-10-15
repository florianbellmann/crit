package azdo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tomasz-tomczyk/crit/internal/forge"
)

// Provider implements Azure DevOps pull-request operations. The organization,
// project, and repository come from the checkout remote or from a PR URL.
type Provider struct{}

type identity struct {
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}

type commitRef struct {
	CommitID string `json:"commitId"`
}

type repositoryRaw struct {
	Name    string `json:"name"`
	WebURL  string `json:"webUrl"`
	Project struct {
		Name string `json:"name"`
	} `json:"project"`
}

type prRaw struct {
	PullRequestID         int            `json:"pullRequestId"`
	Title                 string         `json:"title"`
	Description           string         `json:"description"`
	Status                string         `json:"status"`
	IsDraft               bool           `json:"isDraft"`
	SourceRefName         string         `json:"sourceRefName"`
	TargetRefName         string         `json:"targetRefName"`
	LastMergeSourceCommit commitRef      `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit commitRef      `json:"lastMergeTargetCommit"`
	CreatedBy             identity       `json:"createdBy"`
	CreationDate          string         `json:"creationDate"`
	Repository            repositoryRaw  `json:"repository"`
	ForkSource            *forkSourceRaw `json:"forkSource"`
}

type forkSourceRaw struct {
	Repository repositoryRaw `json:"repository"`
}

type listRaw[T any] struct {
	Value []T `json:"value"`
}

func (Provider) Kind() forge.Kind { return forge.AzureDevOps }

func (Provider) RequireAuth(ctx context.Context, _ forge.RepoContext) error {
	return requireAZ(ctx)
}

// repoCoords resolves the repository from RepoContext.Remote or from origin.
func repoCoords(ctx context.Context, repo forge.RepoContext) (coords, error) {
	remote := repo.Remote
	if remote == "" {
		var err error
		if remote, err = originRemote(ctx); err != nil {
			return coords{}, err
		}
	}
	c, _, err := parseRepoURL(remote)
	return c, err
}

// resolveTarget turns a PR spec (number, URL, or empty for branch detection)
// into repository coordinates and a PR id.
func resolveTarget(ctx context.Context, repo forge.RepoContext, spec string) (coords, int, error) {
	if spec != "" {
		n, fromURL, err := parsePRSpec(spec)
		if err != nil {
			return coords{}, 0, err
		}
		if fromURL != nil {
			return *fromURL, n, nil
		}
		c, err := repoCoords(ctx, repo)
		return c, n, err
	}
	c, err := repoCoords(ctx, repo)
	if err != nil {
		return coords{}, 0, err
	}
	id, err := detectPR(ctx, c)
	return c, id, err
}

func (Provider) Detect(ctx context.Context, repo forge.RepoContext) (forge.ChangeID, error) {
	c, err := repoCoords(ctx, repo)
	if err != nil {
		return forge.ChangeID{}, err
	}
	n, err := detectPR(ctx, c)
	if err != nil {
		return forge.ChangeID{}, err
	}
	return changeID(c, n), nil
}

func detectPR(ctx context.Context, c coords) (int, error) {
	out, err := commandContext(ctx, "git", "branch", "--show-current").Output()
	branch := strings.TrimSpace(string(out))
	if err != nil || branch == "" {
		return 0, fmt.Errorf("cannot detect current branch for Azure DevOps pull request")
	}
	prs, err := listPRs(ctx, c, "searchCriteria.sourceRefName=refs/heads/"+branch)
	if err != nil {
		return 0, err
	}
	if len(prs) == 0 {
		return 0, fmt.Errorf("no active Azure DevOps pull request found for branch %q (try: crit pull <pr-id>)", branch)
	}
	return prs[0].PullRequestID, nil
}

func listPRs(ctx context.Context, c coords, extraQuery ...string) ([]prRaw, error) {
	query := append([]string{"searchCriteria.status=active", "$top=100"}, extraQuery...)
	out, err := c.invoke(ctx, "GET", "pullRequests", nil, query, nil)
	if err != nil {
		return nil, err
	}
	var list listRaw[prRaw]
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing Azure DevOps pull request list: %w", err)
	}
	return list.Value, nil
}

func getPR(ctx context.Context, c coords, n int) (prRaw, error) {
	out, err := c.invoke(ctx, "GET", "pullRequests", []string{"pullRequestId=" + strconv.Itoa(n)}, nil, nil)
	if err != nil {
		return prRaw{}, err
	}
	var pr prRaw
	if err := json.Unmarshal(out, &pr); err != nil {
		return pr, fmt.Errorf("parsing Azure DevOps pull request: %w", err)
	}
	return pr, nil
}

// ParsePRSpec accepts a PR number or URL. A URL carries project/repo in
// ChangeID.Project and the organization URL in ChangeID.Host, so Get can
// resolve it without the checkout remote.
func ParsePRSpec(spec string) (forge.ChangeID, error) {
	n, fromURL, err := parsePRSpec(spec)
	if err != nil || fromURL == nil {
		return forge.ChangeID{Number: n}, err
	}
	return changeID(*fromURL, n), nil
}

func changeID(c coords, n int) forge.ChangeID {
	return forge.ChangeID{Number: n, Project: c.Project + "/" + c.Repo, Host: c.OrgURL}
}

func (Provider) Get(ctx context.Context, repo forge.RepoContext, id forge.ChangeID) (forge.ChangeRequest, error) {
	c, err := repoCoords(ctx, repo)
	if project, repoName, ok := strings.Cut(id.Project, "/"); ok && id.Host != "" {
		c, err = coords{OrgURL: id.Host, Project: project, Repo: repoName}, nil
	}
	if err != nil {
		return forge.ChangeRequest{}, err
	}
	pr, err := getPR(ctx, c, id.Number)
	if err != nil {
		return forge.ChangeRequest{}, err
	}
	baseRepo := repoRef(c.OrgURL, pr.Repository)
	headRepo := baseRepo
	if pr.ForkSource != nil {
		headRepo = repoRef(c.OrgURL, pr.ForkSource.Repository)
	}
	return forge.ChangeRequest{
		ID: changeID(c, pr.PullRequestID), URL: prWebURL(pr), Title: pr.Title, Body: pr.Description,
		State: pr.Status, Draft: pr.IsDraft,
		BaseRefName: branchName(pr.TargetRefName), HeadRefName: branchName(pr.SourceRefName),
		BaseSHA: pr.LastMergeTargetCommit.CommitID, HeadSHA: pr.LastMergeSourceCommit.CommitID,
		BaseRepo: baseRepo, HeadRepo: headRepo, CrossRepository: pr.ForkSource != nil,
		Author: pr.CreatedBy.DisplayName, CreatedAt: pr.CreationDate,
	}, nil
}

func (Provider) ListOpen(ctx context.Context, repo forge.RepoContext) ([]forge.ChangeSummary, error) {
	c, err := repoCoords(ctx, repo)
	if err != nil {
		return nil, err
	}
	prs, err := listPRs(ctx, c)
	if err != nil {
		return nil, err
	}
	result := make([]forge.ChangeSummary, 0, len(prs))
	for _, pr := range prs {
		result = append(result, forge.ChangeSummary{
			ID: changeID(c, pr.PullRequestID), Number: pr.PullRequestID, Title: pr.Title, URL: prWebURL(pr),
			HeadRefName: branchName(pr.SourceRefName), HeadSHA: pr.LastMergeSourceCommit.CommitID,
			BaseRefName: branchName(pr.TargetRefName), Draft: pr.IsDraft, Provider: forge.AzureDevOps,
		})
	}
	return result, nil
}

func (Provider) Pull(ctx context.Context, request forge.PullRequest) (forge.PullResult, error) {
	return runPull(ctx, request)
}

func (Provider) Push(ctx context.Context, request forge.PushRequest) (forge.PushResult, error) {
	return runPush(ctx, request)
}

func (Provider) FetchFile(ctx context.Context, repo forge.RepoContext, _ forge.RepoRef, sha, path string) ([]byte, error) {
	c, err := repoCoords(ctx, repo)
	if err != nil {
		return nil, err
	}
	out, err := c.invoke(ctx, "GET", "items", nil, []string{
		"path=/" + strings.TrimPrefix(path, "/"), "versionDescriptor.version=" + sha,
		"versionDescriptor.versionType=commit", "includeContent=true",
	}, nil)
	if err != nil {
		return nil, err
	}
	var item struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &item); err != nil {
		return nil, fmt.Errorf("parsing Azure DevOps item: %w", err)
	}
	return []byte(item.Content), nil
}

func (Provider) Invalidate(forge.ChangeID) {}

func repoRef(orgURL string, repo repositoryRaw) forge.RepoRef {
	return forge.RepoRef{Project: repo.Project.Name + "/" + repo.Name, Host: orgURL, CloneURL: repo.WebURL}
}

func prWebURL(pr prRaw) string {
	return strings.TrimSuffix(pr.Repository.WebURL, "/") + "/pullrequest/" + strconv.Itoa(pr.PullRequestID)
}

func branchName(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

var _ forge.Provider = Provider{}
