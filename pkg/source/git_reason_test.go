package source

import "testing"

func TestGitReason(t *testing.T) {
	for _, tc := range []struct{ stderr, want string }{
		{"remote: \nremote: ========\nremote: The project you were looking for could not be found or you don't have permission to view it.\nremote: ========\nremote: \n", "The project you were looking for could not be found or you don't have permission to view it."},
		{"warning: redirecting\nfatal: couldn't find remote ref refs/tags/v9.9.9\n", "fatal: couldn't find remote ref refs/tags/v9.9.9"},
		{"remote: HTTP Basic: Access denied.\nfatal: Authentication failed for 'https://git.example.test/x.git/'\n", "fatal: Authentication failed for 'https://git.example.test/x.git/'"},
		{"", ""},
	} {
		if got := gitReason(tc.stderr); got != tc.want {
			t.Errorf("gitReason(%q)\n got %q\nwant %q", tc.stderr, got, tc.want)
		}
	}
}
