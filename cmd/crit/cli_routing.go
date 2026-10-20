package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/tomasz-tomczyk/crit/internal/clicmd"
	"github.com/tomasz-tomczyk/crit/internal/focus"
	"github.com/tomasz-tomczyk/crit/internal/forge"
	"github.com/tomasz-tomczyk/crit/internal/live"
	"github.com/tomasz-tomczyk/crit/internal/preview"
	"github.com/tomasz-tomczyk/crit/internal/session"
)

type positionalRoute int

const (
	positionalRouteReview positionalRoute = iota
	positionalRouteChangeReview
	positionalRouteChangeNumber
	positionalRouteLive
	positionalRoutePreview
)

var (
	runReviewForPositionalCLI  = session.RunReview
	runLiveForPositionalCLI    = runLive
	runPreviewForPositionalCLI = runPreview
	detectForgeKindForCLI      = detectForgeKind
)

var changeFlagByForge = map[forge.Kind]string{
	forge.GitHub:      "--pr",
	forge.GitLab:      "--mr",
	forge.AzureDevOps: "--ado",
}

// routePositionalArgs classifies bare positional crit arguments (no subcommand).
func routePositionalArgs(args []string) positionalRoute {
	if _, ok := changeReviewArgs(args); ok {
		return positionalRouteChangeReview
	}
	if isChangeNumberArg(args) {
		return positionalRouteChangeNumber
	}
	if live.LooksLikeLiveArgs(args) {
		return positionalRouteLive
	}
	if preview.LooksLikePreviewArgs(args) {
		return positionalRoutePreview
	}
	return positionalRouteReview
}

// changeReviewArgs rewrites one GitHub PR, GitLab MR, or Azure DevOps PR URL to explicit review argv.
func changeReviewArgs(args []string) ([]string, bool) {
	if len(args) != 1 {
		return nil, false
	}
	switch {
	case focus.LooksLikePRURL(args[0]):
		return []string{"--pr", args[0]}, true
	case focus.LooksLikeADOPRURL(args[0]):
		return []string{"--ado", args[0]}, true
	case focus.LooksLikeMRURL(args[0]):
		return []string{"--mr", args[0]}, true
	default:
		return nil, false
	}
}

// isChangeNumberArg reports a lone PR/MR number that does not name a file in the working directory.
func isChangeNumberArg(args []string) bool {
	if len(args) != 1 {
		return false
	}
	if n, err := strconv.Atoi(args[0]); err != nil || n <= 0 {
		return false
	}
	_, err := os.Stat(args[0])
	return errors.Is(err, fs.ErrNotExist)
}

// runPositionalCLI dispatches bare positional arguments from main.
func runPositionalCLI(args []string) {
	switch routePositionalArgs(args) {
	case positionalRouteChangeReview:
		changeArgs, _ := changeReviewArgs(args)
		clicmd.Exit(runReviewForPositionalCLI(changeArgs))
	case positionalRouteChangeNumber:
		kind, err := detectForgeKindForCLI(forge.Auto)
		if err != nil {
			clicmd.Exit(fmt.Errorf("detecting forge for change %s: %w", args[0], err))
			return
		}
		clicmd.Exit(runReviewForPositionalCLI([]string{changeFlagByForge[kind], args[0]}))
	case positionalRouteLive:
		runLiveForPositionalCLI(args)
	case positionalRoutePreview:
		runPreviewForPositionalCLI(args)
	default:
		clicmd.Exit(runReviewForPositionalCLI(args))
	}
}
