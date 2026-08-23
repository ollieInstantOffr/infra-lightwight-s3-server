package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

// The credential subcommands exist so a fresh deployment can be used before the
// console does. They also give an operator a way back in if the console is
// unreachable, which is exactly when it is most needed.

const credentialUsage = `Usage:
  s3d                              run the server
  s3d credential create [note]     create an S3 access key pair
  s3d credential list              list credentials
  s3d credential revoke <key-id>   revoke a credential
`

// runCredentialCommand handles "s3d credential ...". It returns false if the
// arguments are not a credential command, so the caller can carry on and start
// the server.
func runCredentialCommand(args []string) (handled bool, err error) {
	if len(args) == 0 || args[0] != "credential" {
		return false, nil
	}
	if len(args) < 2 {
		return true, fmt.Errorf("%s", credentialUsage)
	}

	cfg, err := config.Load()
	if err != nil {
		return true, err
	}
	cipher, err := secrets.NewCipher(cfg.CredentialsKey)
	if err != nil {
		return true, fmt.Errorf("credentials key: %w", err)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return true, err
	}
	defer pool.Close()

	switch args[1] {
	case "create":
		note := ""
		if len(args) > 2 {
			note = args[2]
		}
		return true, createCredential(ctx, pool, cipher, note)
	case "list":
		return true, listCredentials(ctx, pool)
	case "revoke":
		if len(args) < 3 {
			return true, fmt.Errorf("revoke needs an access key id\n\n%s", credentialUsage)
		}
		if err := db.RevokeCredential(ctx, pool, args[2]); err != nil {
			return true, err
		}
		fmt.Printf("Revoked %s. It stops working on the next request.\n", args[2])
		return true, nil
	default:
		return true, fmt.Errorf("unknown credential command %q\n\n%s", args[1], credentialUsage)
	}
}

func createCredential(ctx context.Context, pool *db.Pool, cipher *secrets.Cipher, note string) error {
	cred, err := db.CreateCredential(ctx, pool, cipher, note, nil, db.UnrestrictedGrant())
	if err != nil {
		return err
	}
	// Printed once and never recoverable: only the encrypted form is stored.
	fmt.Printf(`Created an S3 credential. The secret is shown only once.

  AWS_ACCESS_KEY_ID=%s
  AWS_SECRET_ACCESS_KEY=%s

`, cred.AccessKeyID, cred.SecretKey)
	return nil
}

func listCredentials(ctx context.Context, pool *db.Pool) error {
	creds, err := db.ListCredentials(ctx, pool)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		fmt.Println("No credentials yet. Create one with: s3d credential create")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACCESS KEY ID\tCREATED\tLAST USED\tSTATE\tNOTE")
	for _, c := range creds {
		lastUsed := "never"
		if c.LastUsedAt != nil {
			lastUsed = c.LastUsedAt.Format("2006-01-02 15:04")
		}
		state := "active"
		if c.Revoked() {
			state = "revoked"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			c.AccessKeyID, c.CreatedAt.Format("2006-01-02 15:04"), lastUsed, state, c.Description)
	}
	return w.Flush()
}
