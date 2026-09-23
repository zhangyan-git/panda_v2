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
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant, "coupon": Coupon, "coffee_machine": CoffeeMachine, "order": Order, "payment": Payment, "account": Account, "lottery": Lottery, "membership": Membership, "partner": Partner, "legacy": Legacy} {
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
		// lottery-service owns the 抽奖 and 奖品 data and nothing else. The store a
		// campaign is scoped to, the coffee machine it hangs off, the order whose
		// 福卡 paid for a participation, and the user who participated all appear as
		// values (location_id, machine_id, source_order_id, user_id…); the 福卡
		// balance itself lives in account-service and is only ever reached over
		// gRPC, which is why no fortune_card table may appear here. Its own eight
		// tables — lottery_activations, lottery_campaigns, lottery_campaign_prizes,
		// lottery_rounds, lottery_participations, lottery_draws, lottery_wins and
		// lottery_win_events — are deliberately absent from this list, because their
		// references point at each other inside the database.
		"lottery": regexp.MustCompile(`REFERENCES\s+(orders|order_\w+|merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|payment_methods|payments|payment_\w+|devices|drinks|device_drinks|manufacturers|manufacturer_credentials|memberships|membership_plans|users|miniapp_users|user_accounts|account_\w+|fortune_card_\w+|coffee_bean_\w+)\s*\(`),
		// membership-service owns the 会员价权益 and nothing else. The user whose
		// entitlement it is, the order that bought or renewed it, the 代扣协议 that
		// charges it, and the 券模板 that carries a 包月会员's 会员价 all appear as
		// values (user_id, order_id, agreement_id, member_price_coupon_template_id);
		// 会员价 itself is a price on coffee_machine's drinks, and a 会员订单 is
		// order-service's row. Its own four tables — memberships, membership_plans,
		// membership_subscriptions and membership_changes — plus both outbox tables
		// are deliberately absent from this list, because their references point at
		// each other inside the database.
		"membership": regexp.MustCompile(`REFERENCES\s+(orders|order_\w+|merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|payment_methods|payments|payment_\w+|devices|drinks|device_drinks|manufacturers|manufacturer_credentials|users|miniapp_users|user_accounts|account_\w+|fortune_card_\w+|coffee_bean_\w+|lottery_\w+|materials|drink_recipes|recipe_items|warehouses|material_batches|stock_\w+)\s*\(`),
		// partner-service owns the 开放平台 governance rows only — 合作方账号、API 密钥、
		// 调用日志. Everything its keys reach is another service's state: the 商户/门店 a
		// 合作方 is *not*, the orders it queries, the coupons it issues, the memberships
		// it looks up, the admin who issued the key. All of those appear as values
		// (partner code, order_no, user_id, created_by) or not at all — never as foreign
		// keys. Its own three tables are deliberately absent from this list, because
		// partner_api_keys references partner_accounts inside the database.
		//
		// 这条清单是十个域里最长的，不是因为它最特殊，而是因为它**什么都不拥有**：一个
		// 纯治理/转发服务能碰到的外部实体就是全集，漏掉一个名字等于漏掉一种越界写法。
		"partner": regexp.MustCompile(`REFERENCES\s+(orders|order_\w+|merchants|brands|stores|brand_audit_records|store_audit_records|admin_\w+|merchant_users|casbin_rule|coupon_\w+|user_coupons|coupon_templates|coupon_batches|payment_methods|payments|payment_\w+|devices|drinks|device_drinks|manufacturers|manufacturer_credentials|users|miniapp_users|user_accounts|account_\w+|fortune_card_\w+|coffee_bean_\w+|lottery_\w+|materials|drink_recipes|recipe_items|warehouses|material_batches|stock_\w+|memberships|membership_plans|membership_subscriptions|membership_changes)\s*\(`),
	}
	for name, set := range map[string]fs.FS{"identity": Identity, "merchant": Merchant, "coupon": Coupon, "coffee_machine": CoffeeMachine, "order": Order, "payment": Payment, "account": Account, "lottery": Lottery, "membership": Membership, "partner": Partner} {
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
// makes eleven copies of the same DDL, and eleven places to forget.
//
// Each copy sits between a pair of sentinel comments (messageBlockBegin/End), so
// the block can be located inside a file that now holds a whole set's DDL. The
// sentinels are the only thing that moved when the sets were squashed: the DDL
// inside them is untouched, including the differences the eleven already had
// before — coupon writes its two indexes on one line, coffee_machine and account
// carry one extra explanatory comment, order has a third index. Those are
// cosmetic except order's extra message_inbox_lease_idx, which is in that
// database's dump and must stay; normalising them is a separate change.
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
	{Identity, "001_identity.sql", "identity"},
	{Merchant, "001_merchant.sql", "merchant"},
	{Coupon, "001_coupon.sql", "coupon"},
	{CoffeeMachine, "001_coffee_machine.sql", "coffee_machine"},
	{Order, "001_order.sql", "order"},
	{Payment, "001_payment.sql", "payment"},
	{Account, "001_account.sql", "account"},
	{Lottery, "001_lottery.sql", "lottery"},
	{Membership, "001_membership.sql", "membership"},
	// partner-service 只写 outbox（后台治理动作的审计出口），不消费任何事件——inbox 一起
	// 建着，与其它九份逐列相同。
	{Partner, "001_partner.sql", "partner"},
}

// The sentinels bracketing each set's copy of the message-table DDL. They exist so
// that a set's whole DDL can live in one file without the byte-for-byte comparison
// below having to re-parse it — see TestMessageTablesAddLeaseColumnsBeforeIndexes.
const (
	messageBlockBegin = "-- >>> message-tables:begin >>>"
	messageBlockEnd   = "-- <<< message-tables:end <<<"
)

func TestMessageTablesStayInSyncAcrossSets(t *testing.T) {
	for _, table := range []string{"message_outbox", "message_inbox"} {
		t.Run(table, func(t *testing.T) {
			var want []string
			wantLabel := ""
			for _, copy := range messageTableCopies {
				migration := readMigration(t, copy.set, copy.file)
				messageBlock(t, copy.label, migration)
				got := tableColumns(migration, table)
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
//
// identity and merchant are the two sets that keep the ALTER fallback, and they
// must keep it identically — they are the same migration written twice, because
// either database may be the one that predates the lease columns. Before the sets
// were squashed this was asserted on the two whole files, which were single-purpose
// files; now each file holds a whole set, so the comparison is scoped to the
// sentinel block they both carry.
func TestMessageTablesAddLeaseColumnsBeforeIndexes(t *testing.T) {
	identity := messageBlock(t, "identity", readMigration(t, Identity, "001_identity.sql"))
	merchant := messageBlock(t, "merchant", readMigration(t, Merchant, "001_merchant.sql"))
	if identity != merchant {
		t.Error("identity and merchant message-table blocks have drifted apart")
	}
	if !strings.Contains(identity, "ADD COLUMN IF NOT EXISTS lease_until") {
		t.Error("identity/merchant block lost the ALTER fallback for the lease columns")
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

// messageBlock returns the text between a set's sentinel comments, trailing
// whitespace trimmed. It fails the test rather than returning an empty string: a
// missing sentinel means the block is no longer findable, and every assertion
// built on it would quietly become a comparison of two empty strings.
func messageBlock(t *testing.T, label, migration string) string {
	t.Helper()
	begin := strings.Index(migration, messageBlockBegin)
	end := strings.Index(migration, messageBlockEnd)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("%s: message-table sentinels %q / %q not found in order", label, messageBlockBegin, messageBlockEnd)
	}
	return strings.TrimSpace(migration[begin:end])
}

func readMigration(t *testing.T, set fs.FS, name string) string {
	t.Helper()
	data, err := fs.ReadFile(set, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
