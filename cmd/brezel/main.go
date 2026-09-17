package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/buildinfo"
	"github.com/infercrane/brezel/internal/securefile"
)

type client struct {
	base    *url.URL
	token   string
	project string
	http    *http.Client
}

var safeEnvironmentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type commandEvent struct {
	ExecutionID string `json:"execution_id"`
	Type        string `json:"type"`
	Data        string `json:"data"`
	ExitCode    int    `json:"exit_code"`
	Status      string `json:"status"`
	Error       any    `json:"error"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exit *commandExitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "brezel:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("brezel", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	baseURL := global.String("url", env("BREZEL_API_URL", "http://127.0.0.1:8080"), "Brezel API URL")
	tokenFile := global.String("token-file", os.Getenv("BREZEL_SERVICE_TOKEN_FILE"), "protected Brezel service token file")
	project := global.String("project", env("BREZEL_PROJECT", "brezel-default"), "project ID")
	showVersion := global.Bool("version", false, "print build identity")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return nil
		}
		return usageError(err.Error())
	}
	if *showVersion {
		return printVersion(os.Stdout, nil)
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		return usageError("a command is required")
	}
	switch remaining[0] {
	case "help":
		if len(remaining) != 1 {
			return usageError("help does not accept arguments")
		}
		printUsage(os.Stdout)
		return nil
	case "version":
		return printVersion(os.Stdout, remaining[1:])
	case "doctor":
		return doctor(context.Background(), os.Stdout)
	}
	if *tokenFile == "" {
		return errors.New("BREZEL_SERVICE_TOKEN_FILE or --token-file is required; tokens are not accepted in argv or environment values")
	}
	tokenValue, err := readTokenFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("read service token: %w", err)
	}
	c, err := newClient(*baseURL, tokenValue, *project)
	if err != nil {
		return err
	}
	switch remaining[0] {
	case "new":
		return c.newSandbox(remaining[1:])
	case "list":
		return c.listSandboxes(remaining[1:])
	case "run":
		return c.exec(remaining[1:])
	case "put":
		return c.file(append([]string{"put"}, remaining[1:]...))
	case "get":
		return c.file(append([]string{"get"}, remaining[1:]...))
	case "stop", "start", "delete", "inspect":
		action := map[string]string{"stop": "pause", "start": "resume", "delete": "delete", "inspect": "inspect"}[remaining[0]]
		return c.sandbox(append([]string{action}, remaining[1:]...))
	case "open":
		return c.port(append([]string{"open"}, remaining[1:]...))
	case "checkpoint":
		return c.checkpoint(remaining[1:])
	case "workspace":
		return c.workspace(remaining[1:])
	case "environment":
		return c.environment(remaining[1:])
	case "sandbox":
		return c.sandbox(remaining[1:])
	case "exec":
		return c.exec(remaining[1:])
	case "file":
		return c.file(remaining[1:])
	case "port":
		return c.port(remaining[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		return usageError("unknown command " + remaining[0])
	}
}

func (c *client) newSandbox(args []string) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	template := flags.String("template", "base", "built-in or installed environment template")
	fromCheckpoint := flags.String("from", "", "filesystem checkpoint ID")
	ttl := flags.Int64("ttl", 3600, "expiration in seconds")
	standby := flags.Int64("standby-after", 0, "idle seconds before standby")
	internet := flags.Bool("allow-internet", false, "allow unrestricted internet egress")
	var workspaces stringList
	flags.Var(&workspaces, "workspace", "durable workspace ID and mount path as ID:/absolute/path (repeatable)")
	if err := flags.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() != 0 {
		return usageError("new does not accept positional arguments")
	}
	if *fromCheckpoint != "" && *template != "base" {
		return usageError("--from and --template cannot be used together")
	}
	lifecycle := map[string]any{"expires_after_seconds": *ttl}
	if *standby > 0 {
		lifecycle["standby_after_seconds"] = *standby
		lifecycle["standby_checkpoint_kind"] = "full_state"
		lifecycle["auto_resume"] = true
	}
	body := map[string]any{
		"lifecycle": lifecycle,
		"network":   map[string]any{"allow_internet": *internet},
	}
	mounts, err := workspaceMounts(workspaces)
	if err != nil {
		return err
	}
	if len(mounts) > 0 {
		body["workspace_mounts"] = mounts
	}
	if *fromCheckpoint != "" {
		body["checkpoint_id"] = *fromCheckpoint
	} else {
		environment, err := c.ensureEnvironment(context.Background(), *template)
		if err != nil {
			return err
		}
		body["environment_revision"] = environment
	}
	var result struct {
		Resource struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"resource"`
	}
	if err := c.json(context.Background(), http.MethodPost, "/v1/sandboxes", body, &result, randomKey("sandbox")); err != nil {
		return err
	}
	fmt.Printf("%s\t%s\n", result.Resource.ID, result.Resource.State)
	return nil
}

func (c *client) ensureEnvironment(ctx context.Context, template string) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return "", usageError("--template cannot be empty")
	}
	name := template
	if !safeEnvironmentName.MatchString(name) {
		digest := sha256.Sum256([]byte(template))
		name = "brezel-" + hex.EncodeToString(digest[:8])
	}
	body := map[string]any{"name": name, "template": template}
	var result struct {
		Resource struct {
			RevisionID string `json:"revision_id"`
		} `json:"resource"`
	}
	if err := c.json(ctx, http.MethodPost, "/v1/environments", body, &result, stableKey("environment", template)); err != nil {
		return "", err
	}
	if result.Resource.RevisionID == "" {
		return "", errors.New("Brezel returned an empty environment revision")
	}
	return result.Resource.RevisionID, nil
}

func (c *client) listSandboxes(args []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	all := flags.Bool("all", false, "include terminal sandboxes")
	asJSON := flags.Bool("json", false, "write the API response as JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return usageError("list accepts only --all and --json")
	}
	path := "/v1/sandboxes"
	if *all {
		path += "?include_terminal=true"
	}
	var result struct {
		Sandboxes []struct {
			ID                 string    `json:"id"`
			State              string    `json:"state"`
			SourceCheckpointID string    `json:"source_checkpoint_id,omitempty"`
			ExpiresAt          time.Time `json:"expires_at"`
		} `json:"sandboxes"`
	}
	if err := c.json(context.Background(), http.MethodGet, path, nil, &result, ""); err != nil {
		return err
	}
	if *asJSON {
		return prettyJSON(os.Stdout, result)
	}
	if len(result.Sandboxes) == 0 {
		fmt.Fprintln(os.Stdout, "No sandboxes.")
		return nil
	}
	fmt.Fprintln(os.Stdout, "ID\tSTATE\tEXPIRES\tSOURCE")
	for _, sandbox := range result.Sandboxes {
		source := "-"
		if sandbox.SourceCheckpointID != "" {
			source = sandbox.SourceCheckpointID
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", sandbox.ID, sandbox.State, sandbox.ExpiresAt.Format(time.RFC3339), source)
	}
	return nil
}

func (c *client) checkpoint(args []string) error {
	if len(args) > 0 && args[0] == "delete" {
		if len(args) != 2 {
			return usageError("checkpoint delete requires CHECKPOINT_ID")
		}
		var result struct {
			Operation struct {
				State string `json:"state"`
			} `json:"operation"`
		}
		if err := c.json(context.Background(), http.MethodDelete, "/v1/checkpoints/"+url.PathEscape(args[1]), nil, &result, randomKey("checkpoint-delete")); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, result.Operation.State)
		return nil
	}
	if len(args) > 0 && args[0] == "create" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	name := flags.String("name", "checkpoint", "checkpoint name")
	if err := flags.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() != 1 {
		return usageError("checkpoint requires SANDBOX_ID")
	}
	var result struct {
		Resource struct {
			ID string `json:"id"`
		} `json:"resource"`
	}
	path := "/v1/sandboxes/" + url.PathEscape(flags.Arg(0)) + "/checkpoints"
	body := map[string]any{"name": *name, "kind": "filesystem"}
	if err := c.json(context.Background(), http.MethodPost, path, body, &result, randomKey("checkpoint")); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, result.Resource.ID)
	return nil
}

func (c *client) workspace(args []string) error {
	if len(args) == 0 {
		return usageError("workspace requires create, list, inspect, or delete")
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return usageError("workspace create requires NAME")
		}
		var result struct {
			Resource struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"resource"`
		}
		if err := c.json(context.Background(), http.MethodPost, "/v1/workspaces", map[string]any{"name": args[1]}, &result, randomKey("workspace")); err != nil {
			return err
		}
		fmt.Printf("%s\t%s\n", result.Resource.ID, result.Resource.State)
		return nil
	case "list":
		flags := flag.NewFlagSet("workspace list", flag.ContinueOnError)
		all := flags.Bool("all", false, "include terminal workspaces")
		asJSON := flags.Bool("json", false, "write the API response as JSON")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usageError("workspace list accepts only --all and --json")
		}
		path := "/v1/workspaces"
		if *all {
			path += "?include_terminal=true"
		}
		var result struct {
			Workspaces []struct {
				ID        string    `json:"id"`
				Name      string    `json:"name"`
				State     string    `json:"state"`
				UpdatedAt time.Time `json:"updated_at"`
			} `json:"workspaces"`
		}
		if err := c.json(context.Background(), http.MethodGet, path, nil, &result, ""); err != nil {
			return err
		}
		if *asJSON {
			return prettyJSON(os.Stdout, result)
		}
		if len(result.Workspaces) == 0 {
			fmt.Fprintln(os.Stdout, "No workspaces.")
			return nil
		}
		fmt.Fprintln(os.Stdout, "ID\tNAME\tSTATE\tUPDATED")
		for _, workspace := range result.Workspaces {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", workspace.ID, workspace.Name, workspace.State, workspace.UpdatedAt.Format(time.RFC3339))
		}
		return nil
	case "inspect":
		if len(args) != 2 {
			return usageError("workspace inspect requires WORKSPACE_ID")
		}
		var result any
		if err := c.json(context.Background(), http.MethodGet, "/v1/workspaces/"+url.PathEscape(args[1]), nil, &result, ""); err != nil {
			return err
		}
		return prettyJSON(os.Stdout, result)
	case "delete":
		if len(args) != 2 {
			return usageError("workspace delete requires WORKSPACE_ID")
		}
		var result any
		if err := c.json(context.Background(), http.MethodDelete, "/v1/workspaces/"+url.PathEscape(args[1]), nil, &result, randomKey("workspace-delete")); err != nil {
			return err
		}
		return prettyJSON(os.Stdout, result)
	default:
		return usageError("unknown workspace command " + args[0])
	}
}

func workspaceMounts(values []string) ([]map[string]string, error) {
	mounts := make([]map[string]string, 0, len(values))
	for _, value := range values {
		workspaceID, mountPath, ok := strings.Cut(value, ":")
		if !ok || workspaceID == "" || !strings.HasPrefix(mountPath, "/") {
			return nil, usageError("--workspace must use WORKSPACE_ID:/absolute/path")
		}
		mounts = append(mounts, map[string]string{"workspace_id": workspaceID, "path": mountPath})
	}
	return mounts, nil
}

func readTokenFile(path string) (string, error) {
	return securefile.ReadText(path, 16<<10)
}

func doctor(ctx context.Context, destination io.Writer) error {
	type check struct {
		name   string
		value  string
		passed bool
	}
	checks := []check{{name: "operating system", value: runtime.GOOS + "/" + runtime.GOARCH, passed: runtime.GOOS == "linux" && runtime.GOARCH == "amd64"}}
	for _, device := range []string{"/dev/kvm", "/dev/net/tun"} {
		info, err := os.Stat(device)
		checks = append(checks, check{name: device, value: statusValue(err), passed: err == nil && info.Mode()&os.ModeDevice != 0})
	}
	_, cgroupErr := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	checks = append(checks, check{name: "cgroup v2", value: statusValue(cgroupErr), passed: cgroupErr == nil})
	for _, command := range [][]string{{"docker", "version", "--format", "{{.Server.Version}}"}, {"docker", "compose", "version", "--short"}, {"docker", "buildx", "version"}} {
		name := strings.Join(command[:len(command)-1], " ")
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		output, err := exec.CommandContext(commandCtx, command[0], command[1:]...).CombinedOutput()
		cancel()
		value := strings.TrimSpace(string(output))
		if value == "" {
			value = statusValue(err)
		}
		checks = append(checks, check{name: name, value: value, passed: err == nil})
	}
	passed := true
	for _, item := range checks {
		mark := "PASS"
		if !item.passed {
			mark = "FAIL"
			passed = false
		}
		fmt.Fprintf(destination, "%-4s  %-18s %s\n", mark, item.name, item.value)
	}
	if !passed {
		return errors.New("host is not ready for the Firecracker runtime; use a dedicated Linux host with KVM")
	}
	return nil
}

func statusValue(err error) string {
	if err == nil {
		return "available"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	return err.Error()
}

func newClient(rawURL, token, project string) (*client, error) {
	if len(token) < 32 {
		return nil, errors.New("service token must contain at least 32 characters")
	}
	base, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("runtime URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if base.Scheme == "http" && base.Hostname() != "127.0.0.1" && base.Hostname() != "localhost" && base.Hostname() != "::1" {
		return nil, errors.New("remote runtime URL must use HTTPS")
	}
	if project == "" || strings.ContainsAny(project, "\r\n") {
		return nil, errors.New("project is required")
	}
	httpClient := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return &client{base: base, token: token, project: project, http: httpClient}, nil
}

func (c *client) environment(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return usageError("environment requires create")
	}
	flags := flag.NewFlagSet("environment create", flag.ContinueOnError)
	name := flags.String("name", "python", "environment name")
	template := flags.String("template", "", "resolved backend template")
	imageDigest := flags.String("image-digest", "", "immutable image digest")
	if err := flags.Parse(args[1:]); err != nil || *template == "" {
		return usageError("environment create requires --template")
	}
	body := map[string]any{"name": *name, "template": *template}
	if *imageDigest != "" {
		body["image_digest"] = *imageDigest
	}
	var result struct {
		Resource struct {
			RevisionID string `json:"revision_id"`
		} `json:"resource"`
	}
	if err := c.json(context.Background(), http.MethodPost, "/v1/environments", body, &result, randomKey("environment")); err != nil {
		return err
	}
	fmt.Println(result.Resource.RevisionID)
	return nil
}

func (c *client) sandbox(args []string) error {
	if len(args) == 0 {
		return usageError("sandbox requires create, inspect, pause, resume, or delete")
	}
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
		environment := flags.String("environment", "", "environment revision")
		ttl := flags.Int64("ttl", 3600, "expiration in seconds")
		standby := flags.Int64("standby-after", 0, "idle seconds before standby")
		internet := flags.Bool("allow-internet", false, "allow unrestricted internet egress")
		var workspaces stringList
		flags.Var(&workspaces, "workspace", "durable workspace ID and mount path as ID:/absolute/path (repeatable)")
		if err := flags.Parse(args[1:]); err != nil || *environment == "" {
			return usageError("sandbox create requires --environment")
		}
		lifecycle := map[string]any{"expires_after_seconds": *ttl}
		if *standby > 0 {
			lifecycle["standby_after_seconds"] = *standby
			lifecycle["standby_checkpoint_kind"] = "full_state"
			lifecycle["auto_resume"] = true
		}
		body := map[string]any{"environment_revision": *environment, "lifecycle": lifecycle, "network": map[string]any{"allow_internet": *internet}}
		mounts, err := workspaceMounts(workspaces)
		if err != nil {
			return err
		}
		if len(mounts) > 0 {
			body["workspace_mounts"] = mounts
		}
		var result struct {
			Resource struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"resource"`
		}
		if err := c.json(context.Background(), http.MethodPost, "/v1/sandboxes", body, &result, randomKey("sandbox")); err != nil {
			return err
		}
		fmt.Printf("%s\t%s\n", result.Resource.ID, result.Resource.State)
		return nil
	case "inspect":
		if len(args) != 2 {
			return usageError("sandbox inspect requires SANDBOX_ID")
		}
		var result any
		if err := c.json(context.Background(), http.MethodGet, "/v1/sandboxes/"+url.PathEscape(args[1]), nil, &result, ""); err != nil {
			return err
		}
		return prettyJSON(os.Stdout, result)
	case "pause", "resume":
		if len(args) != 2 {
			return usageError("sandbox " + args[0] + " requires SANDBOX_ID")
		}
		var result any
		path := "/v1/sandboxes/" + url.PathEscape(args[1]) + ":" + args[0]
		if err := c.json(context.Background(), http.MethodPost, path, nil, &result, randomKey(args[0])); err != nil {
			return err
		}
		return prettyJSON(os.Stdout, result)
	case "delete":
		if len(args) != 2 {
			return usageError("sandbox delete requires SANDBOX_ID")
		}
		var result any
		if err := c.json(context.Background(), http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(args[1]), nil, &result, randomKey("delete")); err != nil {
			return err
		}
		return prettyJSON(os.Stdout, result)
	default:
		return usageError("unknown sandbox command " + args[0])
	}
}

func (c *client) exec(args []string) error {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	cwd := flags.String("cwd", "", "working directory")
	timeout := flags.Int64("timeout", 300, "command timeout in seconds")
	var envs stringList
	flags.Var(&envs, "env", "environment KEY=VALUE (repeatable)")
	if err := flags.Parse(args); err != nil {
		return usageError(err.Error())
	}
	rest := flags.Args()
	if len(rest) < 2 {
		return usageError("exec requires SANDBOX_ID and command argv")
	}
	environment := map[string]string{}
	for _, item := range envs {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return usageError("--env must use KEY=VALUE")
		}
		environment[key] = value
	}
	body, _ := json.Marshal(map[string]any{"argv": rest[1:], "cwd": *cwd, "env": environment, "timeout_seconds": *timeout})
	request, err := c.request(context.Background(), http.MethodPost, "/v1/sandboxes/"+url.PathEscape(rest[0])+"/commands", bytes.NewReader(body), "")
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeAPIError(response)
	}
	exitCode := 0
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var event commandEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("decode command stream: %w", err)
		}
		switch event.Type {
		case "stdout", "stderr":
			data, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil {
				return errors.New("command stream contained invalid output encoding")
			}
			destination := io.Writer(os.Stdout)
			if event.Type == "stderr" {
				destination = os.Stderr
			}
			if _, err := destination.Write(data); err != nil {
				return err
			}
		case "exited":
			exitCode = event.ExitCode
		case "error":
			return errors.New("command stream ended before a confirmed exit")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read command stream: %w", err)
	}
	if exitCode != 0 {
		return &commandExitError{code: boundedExitCode(exitCode)}
	}
	return nil
}

func (c *client) file(args []string) error {
	if len(args) != 4 || (args[0] != "put" && args[0] != "get") {
		return usageError("file requires put|get SANDBOX_ID GUEST_PATH LOCAL_PATH")
	}
	action, sandboxID, guestPath, localPath := args[0], args[1], args[2], args[3]
	path := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files?path=" + url.QueryEscape(guestPath)
	if action == "put" {
		file, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer file.Close()
		request, err := c.request(context.Background(), http.MethodPut, path, file, "")
		if err != nil {
			return err
		}
		if stat, err := file.Stat(); err == nil {
			request.ContentLength = stat.Size()
		}
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return decodeAPIError(response)
		}
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	request, err := c.request(context.Background(), http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeAPIError(response)
	}
	if localPath == "-" {
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	parent := filepath.Dir(localPath)
	if parent != "." {
		if _, err := os.Stat(parent); err != nil {
			return fmt.Errorf("output directory: %w", err)
		}
	}
	output, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	name := output.Name()
	if _, err := io.Copy(output, response.Body); err != nil {
		output.Close()
		_ = os.Remove(name)
		return err
	}
	return output.Close()
}

func (c *client) port(args []string) error {
	if len(args) == 0 || args[0] != "open" {
		return usageError("port requires open")
	}
	flags := flag.NewFlagSet("port open", flag.ContinueOnError)
	ttl := flags.Int64("ttl", 300, "lease lifetime in seconds")
	if err := flags.Parse(args[1:]); err != nil {
		return usageError(err.Error())
	}
	rest := flags.Args()
	if len(rest) != 2 {
		return usageError("port open requires SANDBOX_ID PORT")
	}
	port, err := strconv.ParseUint(rest[1], 10, 16)
	if err != nil || port == 0 {
		return usageError("PORT must be between 1 and 65535")
	}
	var result struct {
		Path      string    `json:"path"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/v1/sandboxes/" + url.PathEscape(rest[0]) + "/ports/" + strconv.FormatUint(port, 10) + "/leases"
	if err := c.json(context.Background(), http.MethodPost, path, map[string]any{"ttl_seconds": *ttl}, &result, ""); err != nil {
		return err
	}
	reference, err := url.Parse(result.Path)
	if err != nil {
		return errors.New("Brezel returned an invalid preview path")
	}
	fmt.Printf("%s\texpires %s\n", c.base.ResolveReference(reference).String(), result.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (c *client) json(ctx context.Context, method, path string, body any, out any, idempotency string) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	request, err := c.request(requestCtx, method, path, reader, idempotency)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(out)
}

func (c *client) request(ctx context.Context, method, path string, body io.Reader, idempotency string) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(reference).String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Project-ID", c.project)
	request.Header.Set("Accept", "application/json")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	return request, nil
}

func decodeAPIError(response *http.Response) error {
	var payload apiError
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("Brezel API returned %s", response.Status)
	}
	if payload.Error.Code == "" {
		return fmt.Errorf("Brezel API returned %s", response.Status)
	}
	return fmt.Errorf("%s: %s", payload.Error.Code, payload.Error.Message)
}

func prettyJSON(destination io.Writer, value any) error {
	encoder := json.NewEncoder(destination)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func randomKey(prefix string) string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return prefix + "-" + hex.EncodeToString(raw)
}

func stableKey(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

func boundedExitCode(code int) int {
	if code < 1 || code > 255 {
		return 1
	}
	return code
}

func usageError(message string) error {
	printUsage(os.Stderr)
	return errors.New(message)
}

func printUsage(destination io.Writer) {
	fmt.Fprintln(destination, `Usage: brezel [--url URL] [--token-file FILE] [--project ID] COMMAND

Commands:
  help
  version [--json]
  doctor
  new [--template NAME|--from CHECKPOINT_ID] [--workspace ID:/PATH] [--ttl SECONDS] [--standby-after SECONDS]
  run [--cwd PATH] [--timeout SECONDS] [--env KEY=VALUE] SANDBOX_ID COMMAND [ARG...]
  put SANDBOX_ID GUEST_PATH LOCAL_PATH
  get SANDBOX_ID GUEST_PATH LOCAL_PATH_OR_DASH
  list [--all] [--json]
  inspect|stop|start|delete SANDBOX_ID
  open [--ttl SECONDS] SANDBOX_ID PORT
  checkpoint create [--name NAME] SANDBOX_ID
  checkpoint delete CHECKPOINT_ID
  workspace create NAME
  workspace list [--all] [--json]
  workspace inspect|delete WORKSPACE_ID

Advanced:
  environment create --template TEMPLATE [--name NAME] [--image-digest SHA256]
  sandbox create --environment REVISION [--workspace ID:/PATH] [--ttl SECONDS] [--standby-after SECONDS]
  sandbox inspect|pause|resume|delete SANDBOX_ID
  exec [--cwd PATH] [--timeout SECONDS] [--env KEY=VALUE] SANDBOX_ID COMMAND [ARG...]
  file put SANDBOX_ID GUEST_PATH LOCAL_PATH
  file get SANDBOX_ID GUEST_PATH LOCAL_PATH_OR_DASH
  port open [--ttl SECONDS] SANDBOX_ID PORT`)
}

func printVersion(destination io.Writer, args []string) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write build identity as JSON")
	if err := flags.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() != 0 {
		return usageError("version accepts only --json")
	}
	info := buildinfo.Current()
	if *asJSON {
		return prettyJSON(destination, info)
	}
	dirty := ""
	if info.Modified {
		dirty = " (modified)"
	}
	fmt.Fprintf(destination, "brezel %s\nrevision %s%s\nbuilt %s\n", info.Version, info.Revision, dirty, info.BuiltAt)
	return nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type stringList []string

func (v *stringList) String() string { return strings.Join(*v, ",") }
func (v *stringList) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type commandExitError struct{ code int }

func (e *commandExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}
