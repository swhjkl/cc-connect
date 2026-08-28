package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

type workspaceCLIRequest struct {
	Project                string `json:"project"`
	Session                string `json:"session"`
	Worktree               string `json:"worktree,omitempty"`
	ExpectedAgentSessionID string `json:"expected_agent_session_id,omitempty"`
	AgentSessionID         string `json:"agent_session_id,omitempty"`
}

type workspaceCLIEnvelope struct {
	SchemaVersion int  `json:"schema_version"`
	OK            bool `json:"ok"`
	Error         *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func runWorkspace(args []string) {
	if code := runWorkspaceCLI(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runWorkspaceCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printWorkspaceUsageTo(stdout)
		return 0
	}
	sub := args[0]
	if sub != "status" && sub != "route" && sub != "unbind" {
		fmt.Fprintf(stderr, "Error: unknown workspace command %q\n", sub)
		return 2
	}
	var req workspaceCLIRequest
	var dataDir string
	jsonOutput := false
	for i := 1; i < len(args); i++ {
		value := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch args[i] {
		case "--project":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "Error: --project requires a value")
				return 2
			}
			req.Project = v
		case "--session":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "Error: --session requires a value")
				return 2
			}
			req.Session = v
		case "--worktree":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "Error: --worktree requires a value")
				return 2
			}
			req.Worktree = v
		case "--expected-agent-session-id":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "Error: --expected-agent-session-id requires a value")
				return 2
			}
			req.ExpectedAgentSessionID = v
		case "--data-dir":
			v, ok := value()
			if !ok {
				fmt.Fprintln(stderr, "Error: --data-dir requires a value")
				return 2
			}
			dataDir = v
		case "--json":
			jsonOutput = true
		default:
			fmt.Fprintf(stderr, "Error: unknown argument %q\n", args[i])
			return 2
		}
	}
	if req.Project == "" || req.Session == "" || !jsonOutput {
		fmt.Fprintln(stderr, "Error: --project, --session, and --json are required")
		return 2
	}
	if (sub == "route" || sub == "unbind") && req.Worktree == "" {
		fmt.Fprintln(stderr, "Error: --worktree is required")
		return 2
	}
	if sub == "unbind" && req.ExpectedAgentSessionID == "" {
		fmt.Fprintln(stderr, "Error: --expected-agent-session-id is required")
		return 2
	}
	payload, _ := json.Marshal(req)
	sockPath := resolveSocketPath(dataDir)
	client := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sockPath) }}}
	resp, err := client.Post("http://unix/lifecycle/workspace/"+sub, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "Error: lifecycle API unavailable: %v\n", err)
		return 5
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var envelope workspaceCLIEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.SchemaVersion != 1 {
		fmt.Fprintln(stderr, "Error: incompatible lifecycle API response")
		return 5
	}
	_, _ = stdout.Write(body)
	if envelope.OK {
		return 0
	}
	if envelope.Error == nil {
		return 1
	}
	switch envelope.Error.Code {
	case "invalid_argument":
		return 2
	case "not_found":
		return 3
	case "state_conflict":
		return 4
	default:
		return 1
	}
}

func printWorkspaceUsageTo(w io.Writer) {
	fmt.Fprintln(w, strings.TrimSpace(`Usage:
  cc-connect workspace status --project <name> --session <platform:chat:user> --json
  cc-connect workspace route --project <name> --session <platform:chat:user> --worktree <absolute-path> --json
  cc-connect workspace unbind --project <name> --session <platform:chat:user> --worktree <absolute-path> --expected-agent-session-id <exact-native-id> --json

The unbind guard is mandatory. A busy closeout succeeds only when the active
and live native agent-session IDs both exactly match the expected ID; the
response then reports closeout_guard.verified and active_turn_preserved.`))
}

func runLifecycleSessionsCLI(args []string, stdout, stderr io.Writer) int {
	sub := args[0]
	var req workspaceCLIRequest
	var dataDir string
	jsonOutput := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--json" {
			jsonOutput = true
			continue
		}
		if i+1 >= len(args) {
			fmt.Fprintf(stderr, "Error: %s requires a value\n", args[i])
			return 2
		}
		flag, value := args[i], args[i+1]
		i++
		switch flag {
		case "--project":
			req.Project = value
		case "--session":
			req.Session = value
		case "--agent-session-id":
			req.AgentSessionID = value
		case "--data-dir":
			dataDir = value
		default:
			fmt.Fprintf(stderr, "Error: unknown argument %q\n", flag)
			return 2
		}
	}
	if req.Project == "" || req.Session == "" || !jsonOutput {
		fmt.Fprintln(stderr, "Error: --project, --session, and --json are required")
		return 2
	}
	if sub == "attach" && req.AgentSessionID == "" {
		fmt.Fprintln(stderr, "Error: --agent-session-id is required")
		return 2
	}
	payload, _ := json.Marshal(req)
	sockPath := resolveSocketPath(dataDir)
	client := &http.Client{Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sockPath) }}}
	resp, err := client.Post("http://unix/lifecycle/sessions/"+sub, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(stderr, "Error: lifecycle API unavailable: %v\n", err)
		return 5
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var envelope workspaceCLIEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.SchemaVersion != 1 {
		fmt.Fprintln(stderr, "Error: incompatible lifecycle API response")
		return 5
	}
	_, _ = stdout.Write(body)
	if envelope.OK {
		return 0
	}
	if envelope.Error == nil {
		return 1
	}
	switch envelope.Error.Code {
	case "invalid_argument":
		return 2
	case "not_found":
		return 3
	case "state_conflict":
		return 4
	default:
		return 1
	}
}
