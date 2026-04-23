package runner

import "testing"

func TestPluginEnvKey_NamingConventions(t *testing.T) {
	// Plugin settings hit the container as PLUGIN_<UPPER_SNAKE>
	// env vars — the Woodpecker/Drone convention every existing
	// plugin image reads. Verify the key-transform covers the
	// shapes operators write: kebab-case, camelCase, dotted.
	cases := []struct {
		in, want string
	}{
		{"command", "COMMAND"},
		{"node-version", "NODE_VERSION"},
		{"targetEnv", "TARGET_ENV"},
		{"channel.name", "CHANNEL_NAME"},
		{"API_KEY", "API_KEY"},                 // already upper snake
		{"do-the-thing-v2", "DO_THE_THING_V2"}, // digits stay
	}
	for _, c := range cases {
		if got := pluginEnvKey(c.in); got != c.want {
			t.Errorf("pluginEnvKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
