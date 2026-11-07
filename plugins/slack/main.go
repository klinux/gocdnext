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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
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

	if err := postSlack(webhook, channel, template); err != nil {
		// NEVER print the raw webhook: its path carries the secret token.
		fmt.Fprintln(os.Stderr, "slack post failed:", scrubbed(err, webhook))
		os.Exit(1)
	}
	fmt.Println("slack: sent")
}

func postSlack(webhook, channel, template string) error {
	body, err := json.Marshal(map[string]string{
		"channel": channel,
		"text":    template,
	})
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack responded: %s", resp.Status)
	}
	return nil
}

// scrubbed renders a post error WITHOUT the webhook URL, whose path carries the
// secret token. net/http wraps transport failures in *url.Error, whose Error()
// echoes the full URL; collapse it to the operation + underlying error (which
// names the host, not the token-bearing path). As a belt-and-suspenders final
// pass, replace any residual exact occurrence of the webhook string.
func scrubbed(err error, webhook string) string {
	msg := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) {
		msg = fmt.Sprintf("%s: %v", uerr.Op, uerr.Err)
	}
	if webhook != "" {
		msg = strings.ReplaceAll(msg, webhook, "[redacted-webhook]")
	}
	return msg
}
