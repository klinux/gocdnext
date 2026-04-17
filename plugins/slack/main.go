// Command slack-plugin is the reference plugin: posts a message to Slack.
//
// Contract (Woodpecker-compatible):
//   - Settings from the YAML become env vars prefixed PLUGIN_*
//   - Standard CI_* vars are injected by the agent
//   - Exit 0 = success, != 0 = failure
//
// Any container that honors this contract is a valid step. No SDK required —
// this file just happens to be in Go because we eat our own dog food.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	webhook := os.Getenv("PLUGIN_WEBHOOK")
	if webhook == "" {
		fmt.Fprintln(os.Stderr, "PLUGIN_WEBHOOK is required")
		os.Exit(2)
	}
	channel := os.Getenv("PLUGIN_CHANNEL")
	template := os.Getenv("PLUGIN_TEMPLATE")

	if template == "" {
		template = fmt.Sprintf("%s #%s → %s (%s)",
			os.Getenv("CI_PIPELINE"),
			os.Getenv("CI_RUN_COUNTER"),
			os.Getenv("CI_PIPELINE_STATUS"),
			os.Getenv("CI_COMMIT_SHA"),
		)
	}

	body, _ := json.Marshal(map[string]string{
		"channel": channel,
		"text":    template,
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "slack post:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		fmt.Fprintln(os.Stderr, "slack responded:", resp.Status)
		os.Exit(1)
	}
	fmt.Println("slack: sent")
}
