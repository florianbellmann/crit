package azdo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tomasz-tomczyk/crit/internal/config"
	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/session"
	"github.com/tomasz-tomczyk/crit/internal/share"
	"github.com/tomasz-tomczyk/crit/internal/vcs"
)

type filePosition struct {
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

type threadContext struct {
	FilePath       string        `json:"filePath"`
	RightFileStart *filePosition `json:"rightFileStart,omitempty"`
	RightFileEnd   *filePosition `json:"rightFileEnd,omitempty"`
	LeftFileStart  *filePosition `json:"leftFileStart,omitempty"`
	LeftFileEnd    *filePosition `json:"leftFileEnd,omitempty"`
}

type threadComment struct {
	ID              int64    `json:"id"`
	ParentCommentID int64    `json:"parentCommentId"`
	Content         string   `json:"content"`
	Author          identity `json:"author"`
	PublishedDate   string   `json:"publishedDate"`
	CommentType     string   `json:"commentType"`
	IsDeleted       bool     `json:"isDeleted"`
}

type thread struct {
	ID            int64           `json:"id"`
	Status        string          `json:"status"`
	ThreadContext *threadContext  `json:"threadContext"`
	Comments      []threadComment `json:"comments"`
	IsDeleted     bool            `json:"isDeleted"`
}

// liveComments drops deleted and system comments. The first entry is the root.
func (t thread) liveComments() []threadComment {
	live := make([]threadComment, 0, len(t.Comments))
	for _, comment := range t.Comments {
		if !comment.IsDeleted && comment.CommentType != "system" && comment.ID != 0 {
			live = append(live, comment)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].ParentCommentID == 0 && live[j].ParentCommentID != 0 })
	return live
}

// location maps a thread context to Crit's path, line span, and side.
func (t thread) location() (path string, start, end int, side string) {
	tc := t.ThreadContext
	if tc == nil {
		return "", 0, 0, ""
	}
	path = strings.TrimPrefix(tc.FilePath, "/")
	startPos, endPos, side := tc.RightFileStart, tc.RightFileEnd, "new"
	if startPos == nil {
		startPos, endPos, side = tc.LeftFileStart, tc.LeftFileEnd, "old"
	}
	if startPos == nil {
		return "", 0, 0, ""
	}
	start, end = startPos.Line, startPos.Line
	if endPos != nil && endPos.Line >= start {
		end = endPos.Line
	}
	return path, start, end, side
}

// isResolved treats every status except active/pending as resolved.
func isResolved(status string) bool {
	switch strings.ToLower(status) {
	case "", "active", "pending", "unknown":
		return false
	}
	return true
}

func threadStatus(resolved bool) string {
	if resolved {
		return "fixed"
	}
	return "active"
}

func fetchThreads(ctx context.Context, c coords, n int) ([]thread, error) {
	out, err := c.invoke(ctx, "GET", "pullRequestThreads", []string{"pullRequestId=" + strconv.Itoa(n)}, nil, nil)
	if err != nil {
		return nil, err
	}
	var list listRaw[thread]
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing Azure DevOps threads: %w", err)
	}
	return list.Value, nil
}

func runPull(ctx context.Context, request forge.PullRequest) (forge.PullResult, error) {
	if err := requireAZ(ctx); err != nil {
		return forge.PullResult{}, err
	}
	c, n, err := resolveTarget(ctx, request.Repo, request.ChangeSpec)
	if err != nil {
		return forge.PullResult{}, err
	}
	threads, err := fetchThreads(ctx, c, n)
	if err != nil {
		return forge.PullResult{}, err
	}
	critPath, err := reviewPath(request.SessionID, request.OutputDir)
	if err != nil {
		return forge.PullResult{}, err
	}
	cj, err := loadOrInitializeReview(critPath)
	if err != nil {
		return forge.PullResult{}, err
	}
	if err := share.CheckGitHubSyncAllowed(cj, "crit pull"); err != nil {
		return forge.PullResult{}, err
	}
	imported, updated := mergeThreads(&cj, threads, session.ResolvePullScope(&cj))
	if imported+updated == 0 {
		fmt.Printf("No new inline comments found on PR %d\n", n)
		return forge.PullResult{}, nil
	}
	if err := review.SaveCritJSON(critPath, cj); err != nil {
		return forge.PullResult{}, err
	}
	fmt.Printf("Pulled %d comments from PR %d into %s\n", imported, n, critPath)
	fmt.Println("Run 'crit' to view them in the browser.")
	return forge.PullResult{Imported: imported, Updated: updated}, nil
}

// commentKey identifies one ADO comment. Comment IDs are only unique within a
// thread (every root is id 1), so the thread ID is part of the identity.
type commentKey struct{ thread, comment int64 }

func refKey(ref session.RemoteRef) commentKey {
	threadID, _ := strconv.ParseInt(ref.ThreadID, 10, 64)
	return commentKey{threadID, ref.CommentID}
}

func mergeThreads(cj *session.CritJSON, threads []thread, scope session.InheritedScope) (imported, updated int) { //nolint:gocyclo // mirrors gitlab.mergeDiscussions conflict rules
	now := time.Now().UTC().Format(time.RFC3339)
	cj.UpdatedAt = now
	seen := make(map[commentKey]bool)
	pending := make(map[commentKey]bool)
	for _, ref := range session.RemoteDeletesFor(*cj, forge.AzureDevOps) {
		pending[refKey(ref)] = true
	}
	for _, t := range threads {
		if t.IsDeleted {
			continue
		}
		comments := t.liveComments()
		for _, comment := range comments {
			seen[commentKey{t.ID, comment.ID}] = true
		}
		path, startLine, endLine, side := t.location()
		if len(comments) == 0 || path == "" || endLine <= 0 || pending[commentKey{t.ID, comments[0].ID}] {
			continue
		}
		root := comments[0]
		remoteResolved := isResolved(t.Status)
		file := cj.Files[path]
		if file.Comments == nil {
			file.Status = "modified"
			file.Comments = []session.Comment{}
		}
		index := findComment(file.Comments, t.ID, root, startLine, endLine)
		if index < 0 {
			comment := session.StampWithFocus(session.Comment{
				ID: session.RandomCommentID(), StartLine: startLine, EndLine: endLine, Side: side,
				Body: root.Content, Author: root.Author.DisplayName, CreatedAt: root.PublishedDate,
				UpdatedAt: now, Resolved: remoteResolved, AzdoThreadID: t.ID, AzdoCommentID: root.ID,
				AzdoResolved: &remoteResolved, LastPushedBodyHash: bodyHash(root.Content),
			}, scope.AsFocus())
			if remoteResolved {
				comment.ResolvedRound = cj.ReviewRound
			}
			file.Comments = append(file.Comments, comment)
			index = len(file.Comments) - 1
			imported++
		} else {
			comment := &file.Comments[index]
			if comment.LastPushedBodyHash == bodyHash(comment.Body) && comment.Body != root.Content {
				comment.Body = root.Content
				comment.UpdatedAt = now
				updated++
			}
			comment.LastPushedBodyHash = bodyHash(root.Content)
			if comment.AzdoThreadID == 0 {
				comment.AzdoThreadID, comment.AzdoCommentID = t.ID, root.ID
				updated++
			}
			// Remote resolution wins only when the local state has not diverged
			// from the last known remote state.
			if (comment.AzdoResolved == nil || comment.Resolved == *comment.AzdoResolved) && comment.Resolved != remoteResolved {
				comment.Resolved = remoteResolved
				comment.UpdatedAt = now
				comment.ResolvedRound = 0
				if remoteResolved {
					comment.ResolvedRound = cj.ReviewRound
				}
				updated++
			}
			comment.AzdoResolved = &remoteResolved
		}
		for _, reply := range comments[1:] {
			if pending[commentKey{t.ID, reply.ID}] {
				continue
			}
			added, changed := mergeReply(&file.Comments[index], t.ID, reply)
			if added {
				imported++
			} else if changed {
				updated++
			}
		}
		cj.Files[path] = file
	}
	updated += dropDeletedComments(cj, seen)
	updated += dropCompletedDeletes(cj, seen)
	return imported, updated
}

func findComment(comments []session.Comment, threadID int64, root threadComment, start, end int) int {
	for i, comment := range comments {
		if comment.AzdoThreadID == threadID {
			return i
		}
		if comment.AzdoThreadID == 0 && comment.Author == root.Author.DisplayName &&
			comment.StartLine == start && comment.EndLine == end && comment.Body == root.Content {
			return i
		}
	}
	return -1
}

func mergeReply(comment *session.Comment, threadID int64, remote threadComment) (added, changed bool) {
	for i := range comment.Replies {
		reply := &comment.Replies[i]
		if reply.AzdoCommentID == remote.ID {
			if reply.LastPushedBodyHash == bodyHash(reply.Body) && reply.Body != remote.Content {
				reply.Body = remote.Content
				changed = true
			}
			reply.LastPushedBodyHash = bodyHash(remote.Content)
			return false, changed
		}
		if reply.AzdoCommentID == 0 && reply.Author == remote.Author.DisplayName && reply.Body == remote.Content {
			reply.AzdoThreadID, reply.AzdoCommentID = threadID, remote.ID
			reply.LastPushedBodyHash = bodyHash(reply.Body)
			return false, true
		}
	}
	comment.Replies = append(comment.Replies, session.Reply{
		ID: session.RandomReplyID(), Body: remote.Content, Author: remote.Author.DisplayName,
		CreatedAt: remote.PublishedDate, AzdoThreadID: threadID, AzdoCommentID: remote.ID,
		LastPushedBodyHash: bodyHash(remote.Content),
	})
	return true, false
}

func dropDeletedComments(cj *session.CritJSON, seen map[commentKey]bool) int {
	removed := 0
	for path, file := range cj.Files {
		comments := file.Comments[:0]
		for _, comment := range file.Comments {
			if comment.AzdoCommentID != 0 && !seen[commentKey{comment.AzdoThreadID, comment.AzdoCommentID}] {
				removed++
				continue
			}
			replies := comment.Replies[:0]
			for _, reply := range comment.Replies {
				if reply.AzdoCommentID != 0 && !seen[commentKey{reply.AzdoThreadID, reply.AzdoCommentID}] {
					removed++
					continue
				}
				replies = append(replies, reply)
			}
			comment.Replies = replies
			comments = append(comments, comment)
		}
		file.Comments = comments
		cj.Files[path] = file
	}
	return removed
}

func dropCompletedDeletes(cj *session.CritJSON, seen map[commentKey]bool) int {
	refs := session.RemoteDeletesFor(*cj, forge.AzureDevOps)
	remaining := make([]session.RemoteRef, 0, len(refs))
	for _, ref := range refs {
		if seen[refKey(ref)] {
			remaining = append(remaining, ref)
		}
	}
	session.ReplaceRemoteDeletes(cj, forge.AzureDevOps, remaining)
	return len(refs) - len(remaining)
}

func runPush(ctx context.Context, request forge.PushRequest) (forge.PushResult, error) {
	event := request.Event
	if event == "" {
		event = "comment"
	}
	if err := requireAZ(ctx); err != nil {
		return forge.PushResult{}, err
	}
	c, n, err := resolveTarget(ctx, request.Repo, request.ChangeSpec)
	if err != nil {
		return forge.PushResult{}, err
	}
	critPath, cj, err := loadPushReview(request.SessionID, request.OutputDir)
	if err != nil {
		return forge.PushResult{}, err
	}
	if err := share.CheckGitHubSyncAllowed(cj, "crit push"); err != nil {
		return forge.PushResult{}, err
	}
	if cj.ActiveDiffScope == string(session.DiffScopeFullStack) {
		return forge.PushResult{}, fmt.Errorf("switch to Layer diff before posting a platform review")
	}
	pr, err := getPR(ctx, c, n)
	if err != nil {
		return forge.PushResult{}, err
	}
	head := pr.LastMergeSourceCommit.CommitID
	if request.DryRun {
		comments, replies, skipped := countNew(cj, head)
		fmt.Printf("Azure DevOps PR %d: %d new comments, %d new replies, %d skipped; event=%s\n", n, comments, replies, skipped, event)
		return forge.PushResult{}, nil
	}

	// Each step persists IDs as it goes, because ADO has no atomic review
	// publish. A failed step saves partial progress so a rerun does not
	// double-post.
	result := forge.PushResult{}
	steps := []func() error{
		func() (err error) { result.Edited, err = pushEdits(ctx, c, n, &cj); return err },
		func() (err error) { result.Deleted, err = pushDeletes(ctx, c, n, &cj); return err },
		func() (err error) { result.Created, result.Replied, err = pushNew(ctx, c, n, &cj, head); return err },
		func() (err error) { result.Resolved, err = pushResolution(ctx, c, n, &cj); return err },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			if saveErr := review.SaveCritJSON(critPath, cj); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("persisting successful Azure DevOps operations: %w", saveErr))
			}
			return result, err
		}
	}
	if err := review.SaveCritJSON(critPath, cj); err != nil {
		return result, err
	}
	if request.Message != "" {
		payload := map[string]any{"status": "active", "comments": []map[string]any{{"parentCommentId": 0, "content": request.Message, "commentType": "text"}}}
		if _, err := c.invoke(ctx, "POST", "pullRequestThreads", []string{"pullRequestId=" + strconv.Itoa(n)}, nil, payload); err != nil {
			return result, fmt.Errorf("comments posted, but the review message failed: %w", err)
		}
	}
	if vote := voteFor(event); vote != "" {
		if _, err := runAZ(ctx, "repos", "pr", "set-vote", "--org", c.OrgURL, "--id", strconv.Itoa(n), "--vote", vote); err != nil {
			return result, fmt.Errorf("comments posted, but the Azure DevOps vote %q failed: %w", vote, err)
		}
	}
	fmt.Printf("Posted %d comments, %d replies, edited %d, deleted %d, resolved %d on PR %d.\n",
		result.Created, result.Replied, result.Edited, result.Deleted, result.Resolved, n)
	return result, nil
}

func voteFor(event string) string {
	switch event {
	case "approve":
		return "approve"
	case "request-changes":
		return "wait-for-author"
	}
	return ""
}

// pushable reports whether a root comment can be posted as a new ADO thread.
func pushable(comment session.Comment, head string) bool {
	switch {
	case comment.Resolved || comment.DOMAnchor != nil || comment.Drifted || comment.EndLine <= 0:
		return false
	case comment.HeadSHA != "" && head != "" && comment.HeadSHA != head:
		return false
	}
	return comment.AzdoThreadID == 0 && comment.GitHubID == 0 && comment.GitLabNoteID == 0
}

func newReply(reply session.Reply) bool {
	return reply.AzdoCommentID == 0 && reply.GitHubID == 0 && reply.GitLabNoteID == 0
}

func sortedPaths(cj session.CritJSON) []string {
	paths := make([]string, 0, len(cj.Files))
	for path := range cj.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func countNew(cj session.CritJSON, head string) (comments, replies, skipped int) {
	for _, path := range sortedPaths(cj) {
		for _, comment := range cj.Files[path].Comments {
			if pushable(comment, head) {
				comments++
			} else if comment.AzdoThreadID == 0 {
				skipped++
			}
			if comment.AzdoThreadID != 0 {
				for _, reply := range comment.Replies {
					if newReply(reply) {
						replies++
					}
				}
			}
		}
	}
	return comments, replies, skipped
}

func pushNew(ctx context.Context, c coords, n int, cj *session.CritJSON, head string) (created, replied int, err error) {
	prRoute := "pullRequestId=" + strconv.Itoa(n)
	for _, path := range sortedPaths(*cj) {
		file := cj.Files[path]
		for i := range file.Comments {
			comment := &file.Comments[i]
			if pushable(*comment, head) {
				body := session.StripBodyRewriter(comment.Body)
				payload := map[string]any{
					"status":        "active",
					"comments":      []map[string]any{{"parentCommentId": 0, "content": body, "commentType": "text"}},
					"threadContext": newThreadContext(path, comment.StartLine, comment.EndLine, comment.Side),
				}
				out, err := c.invoke(ctx, "POST", "pullRequestThreads", []string{prRoute}, nil, payload)
				if err != nil {
					cj.Files[path] = file
					return created, replied, fmt.Errorf("posting comment %s on %s: %w", comment.ID, path, err)
				}
				var posted thread
				if err := json.Unmarshal(out, &posted); err != nil || len(posted.Comments) == 0 {
					cj.Files[path] = file
					return created, replied, fmt.Errorf("parsing created Azure DevOps thread for comment %s: %v", comment.ID, err)
				}
				resolved := false
				comment.AzdoThreadID, comment.AzdoCommentID, comment.AzdoResolved = posted.ID, posted.Comments[0].ID, &resolved
				comment.LastPushedBodyHash = bodyHash(comment.Body)
				created++
			}
			if comment.AzdoThreadID == 0 {
				continue
			}
			for j := range comment.Replies {
				reply := &comment.Replies[j]
				if !newReply(*reply) {
					continue
				}
				payload := map[string]any{"parentCommentId": comment.AzdoCommentID, "content": session.StripBodyRewriter(reply.Body), "commentType": "text"}
				route := []string{prRoute, "threadId=" + strconv.FormatInt(comment.AzdoThreadID, 10)}
				out, err := c.invoke(ctx, "POST", "pullRequestThreadComments", route, nil, payload)
				if err != nil {
					cj.Files[path] = file
					return created, replied, fmt.Errorf("posting reply %s: %w", reply.ID, err)
				}
				var posted threadComment
				if err := json.Unmarshal(out, &posted); err != nil {
					cj.Files[path] = file
					return created, replied, fmt.Errorf("parsing created Azure DevOps reply %s: %w", reply.ID, err)
				}
				reply.AzdoThreadID, reply.AzdoCommentID = comment.AzdoThreadID, posted.ID
				reply.LastPushedBodyHash = bodyHash(reply.Body)
				replied++
			}
		}
		cj.Files[path] = file
	}
	return created, replied, nil
}

// newThreadContext anchors a comment on the right (new) or left (old) file.
// ADO offsets are 1-based character columns, so the end offset highlights the
// last line only partially.
func newThreadContext(path string, start, end int, side string) threadContext {
	if start <= 0 || start > end {
		start = end
	}
	tc := threadContext{FilePath: "/" + strings.TrimPrefix(path, "/")}
	startPos, endPos := &filePosition{Line: start, Offset: 1}, &filePosition{Line: end, Offset: 1}
	if strings.EqualFold(side, "old") || strings.EqualFold(side, "LEFT") {
		tc.LeftFileStart, tc.LeftFileEnd = startPos, endPos
	} else {
		tc.RightFileStart, tc.RightFileEnd = startPos, endPos
	}
	return tc
}

func pushEdits(ctx context.Context, c coords, n int, cj *session.CritJSON) (int, error) {
	edited := 0
	edit := func(threadID, commentID int64, body string) error {
		route := []string{"pullRequestId=" + strconv.Itoa(n), "threadId=" + strconv.FormatInt(threadID, 10), "commentId=" + strconv.FormatInt(commentID, 10)}
		_, err := c.invoke(ctx, "PATCH", "pullRequestThreadComments", route, nil, map[string]any{"content": session.StripBodyRewriter(body)})
		return err
	}
	for path, file := range cj.Files {
		for i := range file.Comments {
			comment := &file.Comments[i]
			if comment.AzdoCommentID != 0 && comment.LastPushedBodyHash != bodyHash(comment.Body) {
				if err := edit(comment.AzdoThreadID, comment.AzdoCommentID, comment.Body); err != nil {
					cj.Files[path] = file
					return edited, err
				}
				comment.LastPushedBodyHash = bodyHash(comment.Body)
				edited++
			}
			for j := range comment.Replies {
				reply := &comment.Replies[j]
				if reply.AzdoCommentID == 0 || reply.LastPushedBodyHash == bodyHash(reply.Body) {
					continue
				}
				if err := edit(reply.AzdoThreadID, reply.AzdoCommentID, reply.Body); err != nil {
					cj.Files[path] = file
					return edited, err
				}
				reply.LastPushedBodyHash = bodyHash(reply.Body)
				edited++
			}
		}
		cj.Files[path] = file
	}
	return edited, nil
}

func pushResolution(ctx context.Context, c coords, n int, cj *session.CritJSON) (int, error) {
	count := 0
	for path, file := range cj.Files {
		for i := range file.Comments {
			comment := &file.Comments[i]
			if comment.AzdoThreadID == 0 || (comment.AzdoResolved != nil && *comment.AzdoResolved == comment.Resolved) {
				continue
			}
			route := []string{"pullRequestId=" + strconv.Itoa(n), "threadId=" + strconv.FormatInt(comment.AzdoThreadID, 10)}
			if _, err := c.invoke(ctx, "PATCH", "pullRequestThreads", route, nil, map[string]any{"status": threadStatus(comment.Resolved)}); err != nil {
				cj.Files[path] = file
				return count, err
			}
			resolved := comment.Resolved
			comment.AzdoResolved = &resolved
			count++
		}
		cj.Files[path] = file
	}
	return count, nil
}

// pushDeletes drains queued deletions. Deleting a thread root also deletes
// its replies, so a local parent deletion removes the whole remote thread.
func pushDeletes(ctx context.Context, c coords, n int, cj *session.CritJSON) (int, error) {
	refs := session.RemoteDeletesFor(*cj, forge.AzureDevOps)
	remaining := make([]session.RemoteRef, 0, len(refs))
	drained := 0
	for _, ref := range refs {
		threadRoute := []string{"pullRequestId=" + strconv.Itoa(n), "threadId=" + ref.ThreadID}
		out, err := c.invoke(ctx, "GET", "pullRequestThreads", threadRoute, nil, nil)
		if isNotFound(err) {
			drained++
			continue
		}
		var t thread
		if err != nil || json.Unmarshal(out, &t) != nil {
			remaining = append(remaining, ref)
			continue
		}
		ids := []int64{ref.CommentID}
		if live := t.liveComments(); len(live) > 0 && live[0].ID == ref.CommentID {
			ids = ids[:0]
			for i := len(live) - 1; i >= 0; i-- {
				ids = append(ids, live[i].ID)
			}
		}
		failed := false
		for _, id := range ids {
			route := append(append([]string{}, threadRoute...), "commentId="+strconv.FormatInt(id, 10))
			if _, err := c.invoke(ctx, "DELETE", "pullRequestThreadComments", route, nil, nil); err != nil && !isNotFound(err) {
				failed = true
				break
			}
		}
		if failed {
			remaining = append(remaining, ref)
			continue
		}
		drained++
	}
	session.ReplaceRemoteDeletes(cj, forge.AzureDevOps, remaining)
	if len(remaining) > 0 {
		return drained, fmt.Errorf("deleted %d Azure DevOps comments, but %d deletions failed and remain queued", drained, len(remaining))
	}
	return drained, nil
}

func reviewPath(sessionID, outputDir string) (string, error) {
	if sessionID != "" {
		return review.ResolveCommandReviewPathWithSession(sessionID, outputDir, "")
	}
	return review.ResolveReviewPath(outputDir)
}

func loadPushReview(sessionID, outputDir string) (string, session.CritJSON, error) {
	critPath, err := reviewPath(sessionID, outputDir)
	if err != nil {
		return "", session.CritJSON{}, err
	}
	data, err := session.ReadFileShared(session.ReviewPathsFor(critPath).Review)
	if os.IsNotExist(err) {
		return "", session.CritJSON{}, fmt.Errorf("no review file found. Run a crit review first")
	}
	if err != nil {
		return "", session.CritJSON{}, err
	}
	var cj session.CritJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return "", cj, fmt.Errorf("invalid review file: %w", err)
	}
	return critPath, cj, nil
}

func loadOrInitializeReview(critPath string) (session.CritJSON, error) {
	var cj session.CritJSON
	data, err := session.ReadFileShared(session.ReviewPathsFor(critPath).Review)
	if err == nil {
		if err := json.Unmarshal(data, &cj); err != nil {
			return cj, fmt.Errorf("invalid review file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return cj, err
	}
	if cj.Files == nil {
		cj.Files = make(map[string]session.CritJSONFile)
		cj.Branch = vcs.CurrentBranch()
		base := config.LoadConfig("").BaseBranch
		if base == "" {
			base = vcs.DefaultBaseRef()
		}
		cj.BaseRef, _ = vcs.MergeBase(base)
		cj.ReviewRound = 1
	}
	return cj, nil
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}
