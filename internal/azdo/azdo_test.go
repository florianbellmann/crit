package azdo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/session"
	"github.com/tomasz-tomczyk/crit/internal/testutil"
)

func TestParseRepoURLAndPRSpec(t *testing.T) {
	widget := coords{OrgURL: "https://dev.azure.com/acme", Project: "My Platform", Repo: "widget"}
	tests := []struct {
		spec     string
		wantPR   int
		wantRepo *coords
		wantErr  bool
	}{
		{spec: "42", wantPR: 42},
		{spec: "https://dev.azure.com/acme/My%20Platform/_git/widget/pullrequest/42", wantPR: 42, wantRepo: &widget},
		{spec: "https://acme@dev.azure.com/acme/My%20Platform/_git/widget/pullrequest/42?_a=files", wantPR: 42, wantRepo: &widget},
		{spec: "https://acme.visualstudio.com/DefaultCollection/Core/_git/api/pullrequest/7", wantPR: 7,
			wantRepo: &coords{OrgURL: "https://acme.visualstudio.com", Project: "Core", Repo: "api"}},
		{spec: "https://dev.azure.com/acme/My%20Platform/_git/widget", wantErr: true},
		{spec: "https://github.com/acme/widget/pull/42", wantErr: true},
		{spec: "0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			n, repo, err := parsePRSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if n != tt.wantPR || (tt.wantRepo == nil) != (repo == nil) || (repo != nil && *repo != *tt.wantRepo) {
				t.Fatalf("got %d %+v, want %d %+v", n, repo, tt.wantPR, tt.wantRepo)
			}
		})
	}
	for remote, want := range map[string]coords{
		"git@ssh.dev.azure.com:v3/acme/My%20Platform/widget":        widget,
		"git@ssh.dev.azure.com:v3/acme/My%20Platform/_git/widget":   widget,
		"https://acme@dev.azure.com/acme/My%20Platform/_git/widget": widget,
		"acme@vs-ssh.visualstudio.com:v3/acme/Core/api":             {OrgURL: "https://acme.visualstudio.com", Project: "Core", Repo: "api"},
	} {
		if got, _, err := parseRepoURL(remote); err != nil || got != want {
			t.Errorf("parseRepoURL(%q) = %+v, %v; want %+v", remote, got, err, want)
		}
	}
}

// azCall is one recorded az invocation with its --in-file payload decoded.
type azCall struct {
	args    []string
	payload map[string]any
}

func (c azCall) is(method, resource string) bool {
	return slices.Contains(c.args, "invoke") && argAfter(c.args, "--http-method") == method && argAfter(c.args, "--resource") == resource
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// stubAZ fakes the az binary. respond maps one call to its stdout.
func stubAZ(t *testing.T, respond func(azCall) string) *[]azCall {
	t.Helper()
	var calls []azCall
	previous := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := azCall{args: append([]string{name}, args...)}
		if path := argAfter(args, "--in-file"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(data, &call.payload) != nil {
				t.Errorf("unreadable payload for %q: %v", args, err)
			}
		}
		calls = append(calls, call)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAZHelperProcess")
		cmd.Env = append(os.Environ(), "CRIT_AZ_HELPER=1", "CRIT_AZ_STDOUT="+base64.StdEncoding.EncodeToString([]byte(respond(call))))
		return cmd
	}
	t.Cleanup(func() { commandContext = previous })
	binDir := t.TempDir()
	azName, azBody := "az", []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		azName, azBody = "az.cmd", []byte("@echo off\r\nexit /b 0\r\n")
	}
	if err := os.WriteFile(filepath.Join(binDir, azName), azBody, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &calls
}

func TestAZHelperProcess(t *testing.T) {
	if os.Getenv("CRIT_AZ_HELPER") != "1" {
		return
	}
	stdout, _ := base64.StdEncoding.DecodeString(os.Getenv("CRIT_AZ_STDOUT"))
	_, _ = io.WriteString(os.Stdout, string(stdout))
	os.Exit(0)
}

func TestPullThenPushRoundTrip(t *testing.T) {
	testutil.SetHome(t, t.TempDir())
	t.Chdir(t.TempDir())
	outputDir := t.TempDir()
	repo := forge.RepoContext{Remote: "https://dev.azure.com/acme/Platform/_git/widget"}
	threads := `{"count":3,"value":[
	  {"id":11,"status":"active","threadContext":{"filePath":"/main.go","rightFileStart":{"line":3,"offset":1},"rightFileEnd":{"line":4,"offset":1}},
	   "comments":[{"id":1,"parentCommentId":0,"content":"Fix this","author":{"displayName":"Reviewer"},"commentType":"text"},
	               {"id":2,"parentCommentId":1,"content":"Agreed","author":{"displayName":"Other"},"commentType":"text"}]},
	  {"id":12,"status":"active","comments":[{"id":1,"parentCommentId":0,"content":"PR-level note","commentType":"text"}]},
	  {"id":13,"status":"closed","threadContext":{"filePath":"/main.go","leftFileStart":{"line":9,"offset":1}},
	   "comments":[{"id":1,"parentCommentId":0,"content":"Old side","author":{"displayName":"Reviewer"},"commentType":"text","isDeleted":true}]}]}`
	stubAZ(t, func(call azCall) string {
		if call.is("GET", "pullRequestThreads") {
			return threads
		}
		return ""
	})
	result, err := runPull(context.Background(), forge.PullRequest{Repo: repo, ChangeSpec: "7", OutputDir: outputDir})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 {
		t.Fatalf("pull imported %d, want 2 (root + reply; PR-level and deleted threads skipped)", result.Imported)
	}

	critPath, err := review.ResolveReviewPath(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	cj, err := review.LoadCritJSON(critPath)
	if err != nil {
		t.Fatal(err)
	}
	file := cj.Files["main.go"]
	pulled := &file.Comments[0]
	if pulled.AzdoThreadID != 11 || pulled.AzdoCommentID != 1 || pulled.StartLine != 3 || pulled.EndLine != 4 || pulled.Side != "new" {
		t.Fatalf("pulled comment = %+v", *pulled)
	}
	pulled.Resolved = true
	pulled.Replies = append(pulled.Replies, session.Reply{ID: "rp_local", Body: "Done"})
	file.Comments = append(file.Comments, session.Comment{ID: "c_new", StartLine: 10, EndLine: 10, Side: "old", Body: "Why removed?"})
	cj.Files["main.go"] = file
	if err := review.SaveCritJSON(critPath, cj); err != nil {
		t.Fatal(err)
	}

	calls := stubAZ(t, func(call azCall) string {
		switch {
		case call.is("GET", "pullRequests"):
			return `{"pullRequestId":7,"lastMergeSourceCommit":{"commitId":"head"}}`
		case call.is("POST", "pullRequestThreads"):
			return `{"id":20,"comments":[{"id":1}]}`
		case call.is("POST", "pullRequestThreadComments"):
			return `{"id":3}`
		}
		return "{}"
	})
	pushed, err := runPush(context.Background(), forge.PushRequest{Repo: repo, ChangeSpec: "7", OutputDir: outputDir, Event: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Created != 1 || pushed.Replied != 1 || pushed.Resolved != 1 || pushed.Edited != 0 {
		t.Fatalf("push result = %+v", pushed)
	}
	var newThread, reply, vote azCall
	var statuses []any
	for _, call := range *calls {
		switch {
		case call.is("POST", "pullRequestThreads"):
			newThread = call
		case call.is("POST", "pullRequestThreadComments"):
			reply = call
		case call.is("PATCH", "pullRequestThreads"):
			statuses = append(statuses, argAfter(call.args, "--route-parameters"), call.payload["status"])
		case slices.Contains(call.args, "set-vote"):
			vote = call
		}
	}
	tc := newThread.payload["threadContext"].(map[string]any)
	if tc["filePath"] != "/main.go" || tc["leftFileStart"] == nil || tc["rightFileStart"] != nil {
		t.Fatalf("new thread context = %+v", tc)
	}
	if reply.payload["parentCommentId"] != float64(1) || reply.payload["content"] != "Done" || !slices.Contains(reply.args, "threadId=11") {
		t.Fatalf("reply call = %+v", reply)
	}
	// Thread 11 was resolved locally. The new thread 20 is created active and
	// stays active, so only thread 11 changes status.
	if !slices.Contains(statuses, any("fixed")) || argAfter(vote.args, "--vote") != "approve" {
		t.Fatalf("statuses = %v, vote = %v", statuses, vote.args)
	}

	saved, err := review.LoadCritJSON(critPath)
	if err != nil {
		t.Fatal(err)
	}
	comments := saved.Files["main.go"].Comments
	created := comments[slices.IndexFunc(comments, func(c session.Comment) bool { return c.ID == "c_new" })]
	if created.AzdoThreadID != 20 || created.AzdoCommentID != 1 {
		t.Fatalf("created comment IDs not persisted: %+v", created)
	}
	localReply := comments[0].Replies[len(comments[0].Replies)-1]
	if localReply.AzdoCommentID != 3 || localReply.AzdoThreadID != 11 {
		t.Fatalf("reply IDs not persisted: %+v", localReply)
	}
}

func TestMergeThreadsDropsRemotelyDeletedThreadDespiteSharedCommentID(t *testing.T) {
	cj := session.CritJSON{Files: map[string]session.CritJSONFile{"main.go": {Comments: []session.Comment{
		{ID: "c_gone", StartLine: 1, EndLine: 1, Body: "gone", AzdoThreadID: 11, AzdoCommentID: 1},
	}}}}
	// Thread 11 is gone remotely. Thread 12 also has root comment id 1.
	other := thread{ID: 12, Status: "active", ThreadContext: &threadContext{FilePath: "/main.go", RightFileStart: &filePosition{Line: 5}},
		Comments: []threadComment{{ID: 1, Content: "still here", CommentType: "text"}}}
	mergeThreads(&cj, []thread{other}, session.InheritedScope{})
	comments := cj.Files["main.go"].Comments
	if len(comments) != 1 || comments[0].AzdoThreadID != 12 {
		t.Fatalf("comments = %+v, want only thread 12", comments)
	}
}
