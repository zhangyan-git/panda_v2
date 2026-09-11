package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The pre-split chain is frozen: those files were applied by hand, long before a
// runner existed, so editing one would describe a history no database has. A new
// change goes in the identity or merchant set.
func TestLegacySetIsFrozen(t *testing.T) {
	got, err := Versions(Legacy)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"001_init.sql",
		"002_menus.sql",
		"003_merchants.sql",
		"004_brands_stores.sql",
		"005_brands_stores.sql",
		"006_admin_operation_logs.sql",
		"007_admin_permission_view_manage.sql",
		"008_admin_permission_group_labels.sql",
		"009_drop_orphan_merchant_tables.sql",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("legacy set = %v, want %v", got, want)
	}
}

func TestSetsAreUsable(t *testing.T) {
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant, "legacy": Legacy} {
		t.Run(name, func(t *testing.T) {
			versions, err := Versions(set)
			if err != nil {
				t.Fatal(err)
			}
			if len(versions) == 0 {
				t.Fatal("set is empty")
			}
			for _, version := range versions {
				data, err := fs.ReadFile(set, version)
				if err != nil {
					t.Fatal(err)
				}
				// Apply hands the whole file to one Exec and does not understand
				// goose directives, so it refuses a Down section outright rather
				// than creating and dropping a table in the same transaction.
				// Keeping one out of the tree is cheaper than tripping over it.
				if strings.Contains(string(data), "-- +goose Down") {
					t.Errorf("%s carries a goose Down section", version)
				}
			}
		})
	}
}

// The two domains share values (merchant_id, scope_id), never foreign keys: a
// foreign key across a database boundary is not enforceable, and one that is
// written but not enforced is worse than none. Each set names what it must not
// reference, because the other service's tables simply do not exist there.
func TestSetsDoNotCrossTheDatabaseBoundary(t *testing.T) {
	forbidden := map[string]*regexp.Regexp{
		"identity": regexp.MustCompile(`REFERENCES\s+(merchants|brands|stores|brand_audit_records|store_audit_records)\s*\(`),
		"merchant": regexp.MustCompile(`REFERENCES\s+(admin_\w+|merchant_users|casbin_rule)\s*\(`),
	}
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant} {
		t.Run(name, func(t *testing.T) {
			versions, err := Versions(set)
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range versions {
				data, err := fs.ReadFile(set, version)
				if err != nil {
					t.Fatal(err)
				}
				if match := forbidden[name].FindString(string(data)); match != "" {
					t.Errorf("%s crosses the database boundary: %s", version, match)
				}
			}
		})
	}
}

// Both message sets must add the lease columns before indexing them: the ALTERs
// exist to upgrade tables created by the earliest schema, which had no lease
// columns at all, and an index on a column that does not exist yet fails.
func TestMessageTablesAddLeaseColumnsBeforeIndexes(t *testing.T) {
	identity := readMigration(t, Identity, "003_message_outbox_inbox.sql")
	merchant := readMigration(t, Merchant, "002_message_outbox_inbox.sql")
	// The headers differ (each names its own set); the statements must not.
	if stripComments(identity) != stripComments(merchant) {
		t.Error("identity and merchant message migrations have drifted apart")
	}
	for label, migration := range map[string]string{"identity": identity, "merchant": merchant} {
		alter := strings.Index(migration, "ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_until")
		index := strings.Index(migration, "CREATE INDEX IF NOT EXISTS message_outbox_lease_idx")
		if alter < 0 || index < 0 || alter > index {
			t.Errorf("%s: lease column migration must precede lease index creation", label)
		}
	}
}

// stripComments drops whole-line SQL comments so two copies of the same
// migration can be compared on their statements alone.
func stripComments(migration string) string {
	var kept []string
	for _, line := range strings.Split(migration, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func readMigration(t *testing.T, set fs.FS, name string) string {
	t.Helper()
	data, err := fs.ReadFile(set, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
