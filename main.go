package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"gopkg.in/yaml.v3"
)

const (
	realUnpackerBinEnv  = "UNPACKER_BIN"
	defaultUnpackerBin  = "/app/compose-unpacker"
	decryptedSecretsDir = "decrypted-secrets"
	sopsBin             = "/usr/local/bin/sops"
	defaultAgeKeyFile   = "/mnt/stacks/portainer-compose-unpacker/.age-key"
	secretsFileName     = "secrets.enc.yaml"

	// secretOutputModeEnv picks which of the two secret exposure mechanisms
	// this wrapper produces. Set at image build time (Dockerfile ARG/ENV,
	// see SECRET_OUTPUT_MODE build-arg) since Portainer controls the actual
	// `deploy` invocation - there's no argv the caller can attach a flag to.
	secretOutputModeEnv = "SECRET_OUTPUT_MODE"
	modeBoth            = "both"
	modeFile            = "file"
	modeEnv             = "env"
)

// secretOutputMode reads secretOutputModeEnv, defaulting to modeBoth for an
// unset or unrecognized value.
func secretOutputMode() string {
	switch v := os.Getenv(secretOutputModeEnv); v {
	case modeFile, modeEnv, modeBoth:
		return v
	case "":
		return modeBoth
	default:
		fmt.Fprintf(os.Stderr, "entrypoint-wrapper: unrecognized %s=%q, defaulting to %q\n", secretOutputModeEnv, v, modeBoth)
		return modeBoth
	}
}

// This wrapper is the image's ENTRYPOINT. It pre-decrypts secrets.enc.yaml
// (if present next to the compose file) into plain files under
// <destination>/decrypted-secrets/<project>/, so a stack's docker-compose.yml
// can reference them with `secrets: <name>: file: ...`. That resolution happens
// at compose-file load time, before any `provider` plugin (docker-vals included) has a
// chance to run, so it needs its own, separate decryption pass here.
// It then always execs into the real compose-unpacker binary, unmodified.
func main() {
	if len(os.Args) < 2 || os.Args[1] != "deploy" {
		execRealUnpacker()
		return
	}

	args, err := parseDeployArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint-wrapper: could not parse deploy args (%v), skipping secret pre-decryption\n", err)
		execRealUnpacker()
		return
	}

	mode := secretOutputMode()

	secrets, err := decryptSecrets(args, mode != modeEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint-wrapper: secret pre-decryption failed: %v\n", err)
		os.Exit(255)
	}

	if mode != modeFile {
		injectEnvArgs(secrets)
	}

	execRealUnpacker()
}

// injectEnvArgs adds --env KEY=VALUE for every decrypted secret to the args
// forwarded to the real binary, so project.Environment picks them up too
// (composeplugin.go only merges cli.WithEnv(options.Env)/.env into it, never
// the process's own OS environment) — letting a stack compare
// `secrets: <name>: environment: VAR` against `secrets: <name>: file: ...`
// against the same, up-to-date decrypted values. Same trade-off as any other
// environment-based secret: visible in plain text via `docker inspect`/Portainer.
func injectEnvArgs(secrets map[string]string) {
	if len(secrets) == 0 {
		return
	}

	var envArgs []string
	for key, value := range secrets {
		envArgs = append(envArgs, "--env", strings.ToUpper(key)+"="+value)
	}

	newArgs := make([]string, 0, len(os.Args)+len(envArgs))
	newArgs = append(newArgs, os.Args[0], os.Args[1])
	newArgs = append(newArgs, envArgs...)
	newArgs = append(newArgs, os.Args[2:]...)
	os.Args = newArgs
}

func execRealUnpacker() {
	bin := os.Getenv(realUnpackerBinEnv)
	if bin == "" {
		bin = defaultUnpackerBin
	}
	if err := syscall.Exec(bin, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "entrypoint-wrapper: failed to exec %s: %v\n", bin, err)
		os.Exit(255)
	}
}

type deployArgs struct {
	user             string
	password         string
	skipTLSVerify    bool
	gitRepository    string
	reference        string
	projectName      string
	destination      string
	composeFilePaths []string
}

// deployCommand mirrors compose-unpacker's own commands.DeployCommand
// (commands/compose_deploy.go) field-for-field and tag-for-tag, so kong parses
// exactly what the real binary will parse - including `--flag=value` forms,
// short-flag clustering, and any flag we don't otherwise care about (Prune,
// Keep, ForceRecreateStack, Registry) that would otherwise make a naive
// parser choke or silently misread the positionals that follow. It isn't
// imported directly from compose-unpacker to avoid pulling its whole
// dependency tree (portainer/portainer et al.) into this small wrapper.
// Keep this in sync if that struct's flags change.
type deployCommand struct {
	User                     string   `help:"Username for Git authentication." short:"u"`
	Password                 string   `help:"Password or PAT for Git authentication" short:"p"`
	Prune                    bool     `help:"Prune services during deployment" short:"r"`
	Keep                     bool     `help:"Keep stack folder" short:"k"`
	SkipTLSVerify            bool     `help:"Skip TLS verification for git" name:"skip-tls-verify"`
	ForceRecreateStack       bool     `help:"Force to recreate the target stack regardless whether the image hash changes" name:"force-recreate"`
	Env                      []string `help:"OS ENV for stack" example:"key=value"`
	Registry                 []string `help:"Registry credentials" name:"registry"`
	GitRepository            string   `arg:"" help:"Git repository to deploy from." name:"git-repo"`
	Reference                string   `arg:"" help:"Reference of Git repository to deploy from." name:"git-ref"`
	ProjectName              string   `arg:"" help:"Name of the Compose stack." name:"project-name"`
	Destination              string   `arg:"" help:"Path on disk where the Git repository will be cloned." type:"path" name:"destination"`
	ComposeRelativeFilePaths []string `arg:"" help:"Relative path to the Compose file." name:"compose-file-paths"`
}

// parseDeployArgs parses the args that follow "deploy" using kong (the same
// library compose-unpacker itself uses), so this wrapper reads exactly what
// the real binary will read. It only ever inspects the result - os.Args
// itself is forwarded to execRealUnpacker() unchanged (plus any --env flags
// injectEnvArgs adds), so a parse failure here just means secret
// pre-decryption is skipped; the real binary still gets the original args and
// does its own (authoritative) parsing and error reporting.
func parseDeployArgs(args []string) (*deployArgs, error) {
	var cmd deployCommand
	parser, err := kong.New(&cmd,
		kong.Name("deploy"),
		kong.Exit(func(int) {}),
		kong.Writers(io.Discard, io.Discard),
	)
	if err != nil {
		return nil, fmt.Errorf("building arg parser: %w", err)
	}
	if _, err := parser.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing deploy args: %w", err)
	}

	return &deployArgs{
		user:          cmd.User,
		password:      cmd.Password,
		skipTLSVerify: cmd.SkipTLSVerify,
		gitRepository: cmd.GitRepository,
		reference:     cmd.Reference,
		projectName:   cmd.ProjectName,
		// The wrapper still clones into its own scratch directory (not this
		// one), but needs this real host mount root to place decrypted secret
		// files somewhere the daemon can bind from - see decryptSecrets.
		destination:      cmd.Destination,
		composeFilePaths: cmd.ComposeRelativeFilePaths,
	}, nil
}

// decryptSecrets clones the stack's repo and decrypts secrets.enc.yaml if
// present, returning the key/value pairs so the caller can expose them as
// --env flags for the real binary. When writeFiles is true (mode "file" or
// "both"), it also writes each key to
// <destination>/decrypted-secrets/<project>/<key> - skip this with mode "env"
// to avoid leaving plaintext files on the host at all.
//
// Unlike an environment-sourced secret (whose content Compose embeds directly
// via the API - see the "Mounts": [] observation in docker inspect), a
// file-sourced secret is a genuine bind mount: the daemon resolves the path
// itself, on the real host, so it must exist there - a path only visible
// inside this container (e.g. under /run) fails with "bind source path does
// not exist". `destination` is the one part of compose-unpacker's own working
// directory that's a real, stable host mount (see MakeWorkingDir in
// exec/utils.go: mountPath = destination/stacks/<project>, wiped and
// recreated on every deploy) - writing to a sibling directory here keeps the
// files on that same real mount without colliding with that wipe.
func decryptSecrets(args *deployArgs, writeFiles bool) (map[string]string, error) {
	tmpDir, err := os.MkdirTemp("", "cuv-secrets-*")
	if err != nil {
		return nil, fmt.Errorf("creating scratch clone dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneOpts := &git.CloneOptions{
		URL:             args.gitRepository,
		ReferenceName:   plumbing.ReferenceName(args.reference),
		Depth:           1,
		Tags:            git.NoTags,
		InsecureSkipTLS: args.skipTLSVerify,
	}
	if args.user != "" && args.password != "" {
		cloneOpts.Auth = &http.BasicAuth{Username: args.user, Password: args.password}
	}

	if _, err := git.PlainCloneContext(context.Background(), tmpDir, false, cloneOpts); err != nil {
		return nil, fmt.Errorf("cloning repository for secret pre-decryption: %w", err)
	}

	if len(args.composeFilePaths) == 0 {
		return nil, nil
	}
	composeDir := filepath.Dir(filepath.Join(tmpDir, args.composeFilePaths[0]))

	secretsFile := filepath.Join(composeDir, secretsFileName)
	if _, err := os.Stat(secretsFile); errors.Is(err, os.ErrNotExist) {
		return nil, nil // this stack doesn't use SOPS-encrypted secrets
	}

	ageKeyFile := resolveAgeKeyFile(composeDir)

	decrypted, err := runSops(secretsFile, ageKeyFile)
	if err != nil {
		return nil, fmt.Errorf("decrypting %s: %w", secretsFile, err)
	}

	values := map[string]any{}
	if err := yaml.Unmarshal(decrypted, &values); err != nil {
		return nil, fmt.Errorf("parsing decrypted secrets: %w", err)
	}

	var outDir string
	if writeFiles {
		outDir = filepath.Join(args.destination, decryptedSecretsDir, args.projectName)
		if err := os.MkdirAll(outDir, 0700); err != nil {
			return nil, fmt.Errorf("creating %s: %w", outDir, err)
		}
	}

	secrets := make(map[string]string, len(values))
	for key, val := range values {
		if key == "sops" {
			continue // SOPS' own metadata block, not a secret
		}
		strVal, ok := val.(string)
		if !ok {
			continue
		}
		if writeFiles {
			if err := os.WriteFile(filepath.Join(outDir, key), []byte(strVal), 0400); err != nil {
				return nil, fmt.Errorf("writing secret %q: %w", key, err)
			}
		}
		secrets[key] = strVal
	}

	return secrets, nil
}

// resolveAgeKeyFile mirrors the SOPS_AGE_KEY_FILE resolution compose-unpacker
// itself ends up with via project.Environment (.env next to the compose file
// takes precedence), since this wrapper runs before that's ever loaded.
func resolveAgeKeyFile(composeDir string) string {
	envFile := filepath.Join(composeDir, ".env")
	if data, err := os.ReadFile(envFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "SOPS_AGE_KEY_FILE="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	if v := os.Getenv("SOPS_AGE_KEY_FILE"); v != "" {
		return v
	}
	return defaultAgeKeyFile
}

func runSops(secretsFile, ageKeyFile string) ([]byte, error) {
	cmd := exec.Command(sopsBin, "-d", secretsFile)
	cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+ageKeyFile)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out, nil
}
