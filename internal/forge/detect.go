package forge

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseKind validates a configured or CLI-supplied forge value.
func ParseKind(value string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(value))) {
	case "", Auto:
		return Auto, nil
	case GitHub:
		return GitHub, nil
	case GitLab:
		return GitLab, nil
	case AzureDevOps:
		return AzureDevOps, nil
	default:
		return "", fmt.Errorf("invalid forge %q (expected auto, github, gitlab, or azuredevops)", value)
	}
}

// IsAzureDevOpsHost recognizes Azure DevOps Services HTTPS and SSH hosts.
func IsAzureDevOpsHost(host string) bool {
	host = strings.ToLower(host)
	return host == "dev.azure.com" || host == "ssh.dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com")
}

// DetectKind applies an explicit selection first, then recognizes common
// remote hosts. Unknown hosts retain GitHub as Crit's compatibility default.
func DetectKind(explicit string, remote string) (Kind, error) {
	kind, err := ParseKind(explicit)
	if err != nil || kind != Auto {
		return kind, err
	}
	host := remoteHost(remote)
	if IsAzureDevOpsHost(host) {
		return AzureDevOps, nil
	}
	if strings.Contains(host, "gitlab") {
		return GitLab, nil
	}
	return GitHub, nil
}

// RemoteHost returns the hostname from HTTPS/SSH/SCP-style remotes.
func RemoteHost(remote string) string { return remoteHost(remote) }

func remoteHost(remote string) string {
	remote = strings.TrimSpace(remote)
	if u, err := url.Parse(remote); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	// SCP-style SSH remote: git@example.com:group/project.git.
	if at := strings.LastIndex(remote, "@"); at >= 0 {
		remote = remote[at+1:]
	}
	if colon := strings.IndexByte(remote, ':'); colon >= 0 {
		return strings.ToLower(remote[:colon])
	}
	return ""
}
