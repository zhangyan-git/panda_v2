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
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant, "coupon": Coupon, "coffee_machine": CoffeeMachine, "order": Order, "payment": Payment, "account": Account, "legacy": Legacy} {
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
		"identity":       regexp.MustCompile(`REFERENCES\s+(merchants|brands|stores|brand_audit_records|store_audit_records)\s*\(`),
		"merchant":       regexp.MustCompile(`REFERENCES\s+(admin_\w+|merchant_users|casbin_rule)\s*\(`),
		"coupon":         regexp.MustCompile(`REFERENCES\s+(merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule)\s*\(`),
		"coffee_machine": regexp.MustCompile(`REFERENCES\s+(merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|payment_methods)\s*\(`),
		// order-service owns no other service's state: users, stores, devices,
		// drinks, coupons, memberships and 账户 all appear in its tables as values
		// (user_id, store_id, coupon_id…), never as foreign keys. Its own two
		// tables — order_lines and order_after_sales — are deliberately absent
		// from this list, because those references are inside the database.
		"order": regexp.MustCompile(`REFERENCES\s+(merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|payment_methods|devices|drinks|device_drinks|manufacturers|memberships|membership_plans|users|user_accounts)\s*\(`),
		// payment-service owns no other service's state either: the order it pays
		// for, the user paying, the account entry an 咖啡豆 payment debits, the
		// coupon and membership a refund restores, and the admin who clicked 人工退款
		// all appear as values (order_no, user_id, account_entry_id…). Its own
		// tables — payments, payment_fundings and the rest — are deliberately absent
		// from this list, because those references are inside the database.
		"payment": regexp.MustCompile(`REFERENCES\s+(orders|order_\w+|merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|devices|drinks|device_drinks|manufacturers|manufacturer_credentials|memberships|membership_plans|users|miniapp_users|user_accounts|account_\w+)\s*\(`),
		// account-service holds a user's 福卡 balance, their 咖啡豆 balance and the
		// two ledgers behind them. The user whose balance it is, the order that
		// granted or was paid for with it, and the after-sale that froze or reversed
		// it appear as values (user_id, reference_no, after_sale_no); its own five
		// tables — fortune_card_accounts, fortune_card_entries, fortune_card_freezes,
		// coffee_bean_accounts and coffee_bean_entries — are deliberately absent from
		// this list, because their references point at each other inside the
		// database.
		"account": regexp.MustCompile(`REFERENCES\s+(orders|order_\w+|merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|payment_methods|payments|payment_\w+|devices|drinks|device_drinks|manufacturers|manufacturer_credentials|memberships|membership_plans|users|miniapp_users|user_accounts)\s*\(`),
	}
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant, "coupon": Coupon, "coffee_machine": CoffeeMachine, "order": Order, "payment": Payment, "account": Account} {
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

// Every database needs its own pair of message tables — the outbox is written in
// the same transaction as the business row, so it cannot be a shared table. That
// makes seven copies of the same DDL, and seven places to forget.
//
// The copies are compared on their CREATE TABLE column lists, in order. Comments,
// formatting, and the ALTER block the identity and merchant sets carry (it exists
// to upgrade tables built by the earliest schema) deliberately do not take part:
// a fresh database writes the lease columns into CREATE TABLE, an existing one
// adds them afterwards, and both are correct. What must not differ is the column
// set itself — a relay reading a column that one database never got fails only
// there, while every other set's tests stay green.
var messageTableCopies = []struct {
	set   fs.FS
	file  string
	label string
}{
	{Identity, "003_message_outbox_inbox.sql", "identity"},
	{Merchant, "002_message_outbox_inbox.sql", "merchant"},
	{Coupon, "001_coupon_core.sql", "coupon"},
	{CoffeeMachine, "001_coffee_machine_core.sql", "coffee_machine"},
	{Order, "001_order_core.sql", "order"},
	{Payment, "001_payment_core.sql", "payment"},
	{Account, "001_account_core.sql", "account"},
}

func TestMessageTablesStayInSyncAcrossSets(t *testing.T) {
	for _, table := range []string{"message_outbox", "message_inbox"} {
		t.Run(table, func(t *testing.T) {
			var want []string
			wantLabel := ""
			for _, copy := range messageTableCopies {
				got := tableColumns(readMigration(t, copy.set, copy.file), table)
				if len(got) == 0 {
					t.Fatalf("%s: %s not found in %s", copy.label, table, copy.file)
				}
				if want == nil {
					want, wantLabel = got, copy.label
					continue
				}
				if strings.Join(got, "|") != strings.Join(want, "|") {
					t.Errorf("%s %s has drifted from %s:\n %v\n %v", copy.label, table, wantLabel, got, want)
				}
			}
		})
	}
}

// The lease columns must exist before the index on them: the ALTER block exists to
// upgrade tables built by the earliest schema, which had no lease columns at all,
// and creating an index on a column that does not exist yet fails outright. In the
// two fresh-database sets the column is part of CREATE TABLE instead, so the test
// asks where lease_until is first mentioned rather than looking for an ALTER.
func TestMessageTablesAddLeaseColumnsBeforeIndexes(t *testing.T) {
	identity := readMigration(t, Identity, "003_message_outbox_inbox.sql")
	merchant := readMigration(t, Merchant, "002_message_outbox_inbox.sql")
	// The headers differ (each names its own set); the statements must not.
	if stripComments(identity) != stripComments(merchant) {
		t.Error("identity and merchant message migrations have drifted apart")
	}
	for _, copy := range messageTableCopies {
		migration := readMigration(t, copy.set, copy.file)
		// The outbox block always precedes the inbox one, so the first mention of
		// the column is the outbox's.
		column := strings.Index(migration, "lease_until")
		index := strings.Index(migration, "message_outbox_lease_idx")
		if column < 0 || index < 0 || column > index {
			t.Errorf("%s: lease_until must be introduced before message_outbox_lease_idx (column at %d, index at %d)", copy.label, column, index)
		}
	}
}

// tableColumns returns the column definitions of one CREATE TABLE, in order and
// without the trailing commas. One column per line is the convention these
// copies already follow, so anything else here is a parse failure worth seeing.
func tableColumns(migration, table string) []string {
	statement := regexp.MustCompile(`(?s)CREATE TABLE (?:IF NOT EXISTS )?` + table + `\s*\((.*?)\n\);`)
	match := statement.FindStringSubmatch(migration)
	if match == nil {
		return nil
	}
	var columns []string
	for _, line := range strings.Split(match[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		columns = append(columns, strings.TrimSuffix(line, ","))
	}
	return columns
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
