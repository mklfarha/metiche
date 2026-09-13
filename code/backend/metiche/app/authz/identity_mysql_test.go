package authz

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"

	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/enums"
)

// The board gate and the MCP surface must agree that a retired agent's token
// names nobody. app/mcp proves its half in TestIntegrationRetiredAgentTokenIsDead;
// this is the board's half, through the production Guard, on a real MySQL.
//
// It needs METICHE_TEST_MYSQL_DSN pointing at a database with create.sql and
// deploy/sql/2026-09-agent-token-hash.sql applied — the same one the app/mcp
// integration suite uses. No DSN is committed anywhere in this repository.
func TestRetiredAgentTokenIsDeadAtTheBoardGate(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("METICHE_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("skipped: METICHE_TEST_MYSQL_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening MySQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipped: METICHE_TEST_MYSQL_DSN is set but the database is unreachable: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("SET FOREIGN_KEY_CHECKS = 0")
	for _, tbl := range []string{"session", "invite", "agent", "member", "team", "account"} {
		exec("TRUNCATE TABLE `" + tbl + "`")
	}
	exec("SET FOREIGN_KEY_CHECKS = 1")

	accountID := uuid.Must(uuid.NewV4())
	teamID := uuid.Must(uuid.NewV4())
	agentID := uuid.Must(uuid.NewV4())
	slug := "gate-" + teamID.String()[:8]

	token, agentHash, err := metichemcp.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	// The account's own hash is of a token nobody holds, as on every v4 account.
	_, accountHash, err := metichemcp.MintToken()
	if err != nil {
		t.Fatal(err)
	}

	exec("INSERT INTO `account` (`id`,`key`,`display_name`,`token_hash`,`identity_provider`,`status`) VALUES (?,?,?,?,?,?)",
		accountID.String(), strings.ReplaceAll(accountID.String(), "-", ""), "Ana", accountHash,
		enums.IDENTITY_PROVIDER_NONE, enums.RECORD_STATUS_ACTIVE)
	exec("INSERT INTO `team` (`id`,`name`,`slug`,`status`,`visibility`) VALUES (?,?,?,?,?)",
		teamID.String(), "Gate", slug, enums.RECORD_STATUS_ACTIVE, enums.TEAM_VISIBILITY_PRIVATE)
	exec("INSERT INTO `member` (`id`,`team_uuid`,`key`,`display_name`,`role`,`status`,`account_uuid`) VALUES (?,?,?,?,?,?,?)",
		uuid.Must(uuid.NewV4()).String(), teamID.String(), "ana", "Ana", enums.MEMBER_ROLE_MEMBER,
		enums.RECORD_STATUS_ACTIVE, accountID.String())
	exec("INSERT INTO `agent` (`id`,`key`,`label`,`client_key`,`status`,`account_uuid`,`token_hash`) VALUES (?,?,?,?,?,?,?)",
		agentID.String(), "A-1", "claude", "laptop-claude", enums.AGENT_STATUS_ACTIVE, accountID.String(), agentHash)

	guard := NewGuard(db)
	if _, err := guard.Authorize(ctx, slug, token); err != nil {
		t.Fatalf("an ACTIVE agent's token on its own private team was denied: %v", err)
	}

	exec("UPDATE `agent` SET `status` = ? WHERE `id` = ?", enums.AGENT_STATUS_RETIRED, agentID.String())

	if _, err := guard.Authorize(ctx, slug, token); !errors.Is(err, ErrDenied) {
		t.Fatalf("a RETIRED agent's token on its private team: %v, want ErrDenied", err)
	}
}
