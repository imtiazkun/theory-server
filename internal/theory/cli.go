package theory

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const usage = `Usage:
  theory-server serve [--addr :8080]
  theory-server vault create <name>
  theory-server member add --name "<name>" --email <email>
  theory-server member remove --email <email>
  theory-server token create --email <email>
  theory-server token revoke --email <email>
  theory-server lock break <vault>
  theory-server hook pre-receive

All state lives in the data folder: $THEORY_DATA, or /data when it isn't set.
`

// DataDir is the folder that holds all the server's state (GIT-10).
func DataDir() string {
	if d := os.Getenv("THEORY_DATA"); d != "" {
		return d
	}
	return "/data"
}

func openStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return OpenStore(filepath.Join(dataDir, "theory.db"))
}

// Main runs a command and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == "hook" {
		if len(args) != 2 || args[1] != "pre-receive" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		msg, err := PreReceive(".", os.Getenv(PusherEnv), stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Rejected: the server couldn't check this push: %v\n", err)
			return 1
		}
		if msg != "" {
			fmt.Fprintln(stderr, msg)
			return 1
		}
		return 0
	}
	if err := run(args, stdout); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(stderr, usage)
			return 2
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage")

func run(args []string, stdout io.Writer) error {
	dataDir := DataDir()
	if args[0] == "serve" {
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		addr := fs.String("addr", ":8080", "address to listen on")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errUsage
		}
		return serve(dataDir, *addr)
	}
	if len(args) < 2 {
		return errUsage
	}
	cmd := args[0] + " " + args[1]
	switch cmd {
	case "vault create":
		if len(args) != 3 {
			return errUsage
		}
		if err := CreateVault(dataDir, args[2]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created the vault %s.\n", args[2])
		return nil
	case "lock break":
		if len(args) != 3 {
			return errUsage
		}
		if !VaultExists(dataDir, args[2]) {
			return fmt.Errorf("There is no vault named %s.", args[2])
		}
		return withStore(dataDir, func(s *Store) error {
			broke, err := s.BreakLock(args[2])
			if err != nil {
				return err
			}
			if broke {
				fmt.Fprintf(stdout, "Broke the compile lock of %s. The marker didn't move.\n", args[2])
			} else {
				fmt.Fprintf(stdout, "Nobody holds the compile lock of %s.\n", args[2])
			}
			return nil
		})
	case "member add", "member remove", "token create", "token revoke":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		name := fs.String("name", "", "member name")
		email := fs.String("email", "", "member email")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || *email == "" {
			return errUsage
		}
		return withStore(dataDir, func(s *Store) error {
			switch cmd {
			case "member add":
				if err := s.AddMember(*name, *email); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "Added %s <%s>.\n", *name, *email)
			case "member remove":
				if err := s.RemoveMember(*email); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "Removed %s and revoked their tokens. Their commits stay in every vault.\n", *email)
			case "token create":
				token, err := s.CreateToken(*email)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "%s\nThis token is shown only once.\n", token)
			case "token revoke":
				n, err := s.RevokeTokens(*email)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "Revoked %d token(s) of %s.\n", n, *email)
			}
			return nil
		})
	}
	return errUsage
}

func withStore(dataDir string, f func(*Store) error) error {
	s, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}

func serve(dataDir, addr string) error {
	store, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := InstallHooks(dataDir); err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	s := &Server{DataDir: dataDir, Store: store, HookBin: bin, Now: time.Now}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	log.Printf("theory-server listening on %s, data in %s", addr, dataDir)
	return srv.ListenAndServe()
}
