// Package migrations embeds the SQL migration sets this repository ships.
//
// The SQL lives outside every Go module tree, so it is embedded here and handed
// to platform/database/migrate as an fs.FS. Twelve sets exist:
//
//   - Legacy: the pre-split single-database chain (001…009). It is kept
//     byte-for-byte as it was applied, so a database that predates the split can
//     still be described exactly as it was built.
//   - Identity: user-service's database.
//   - Merchant: merchant-service's database.
//   - Coupon: coupon-service's database.
//   - CoffeeMachine: coffee-machine-service's database.
//   - Order: order-service's database.
//   - Payment: payment-service's database.
//   - Account: account-service's database.
//   - Lottery: lottery-service's database.
//   - Membership: membership-service's database. The DDL is applied ahead of the
//     service: 会员价权益、套餐、有效期与续费期次 first, so the
//     shape can be reviewed before any Go code depends on it. It owns the
//     entitlement only — a 会员订单 is order-service's row, and 会员价 is a price
//     on coffee_machine's drinks.
//   - Partner: partner-service's database — the 开放平台 domain. It owns three
//     things and nothing else: who may call the open API (合作方账号), which keys
//     they hold (密钥、密文签名密钥、启停、过期、IP 白名单、限流) and what they called
//     (调用日志). The 合作方 is **not** a merchant: the users, orders, coupons and
//     memberships its keys reach are other services' rows, reached over gRPC.
//
// Identity, Merchant, Coupon, CoffeeMachine, Order, Payment, Account, Lottery,
// Membership, and Partner each encode the state their service owns. No migration set creates a
// foreign key into another service's database.
//
// Identity and Merchant each encode the state the legacy chain converges to for
// their own domain — no cross-database foreign key, no table that moved to the
// other service — and then carry only their own history. A fresh database runs
// one set from the beginning. A database that predates the split is adopted with
// `panda-migrate adopt`, which records the versions it already satisfies and
// applies whatever follows.
//
// The two domains never reference each other across the boundary: the shared
// identity is a value (merchant_id, scope_id), not a foreign key.
// TestSetsDoNotCrossTheDatabaseBoundary fails if a set ever reintroduces one.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.sql
var legacyFiles embed.FS

//go:embed identity/*.sql
var identityFiles embed.FS

//go:embed merchant/*.sql
var merchantFiles embed.FS

//go:embed coupon/*.sql
var couponFiles embed.FS

//go:embed coffee_machine/*.sql
var coffeeMachineFiles embed.FS

//go:embed order/*.sql
var orderFiles embed.FS

//go:embed payment/*.sql
var paymentFiles embed.FS

//go:embed account/*.sql
var accountFiles embed.FS

//go:embed lottery/*.sql
var lotteryFiles embed.FS

//go:embed membership/*.sql
var membershipFiles embed.FS

//go:embed partner/*.sql
var partnerFiles embed.FS

// Legacy is the pre-split single-database migration chain.
var Legacy fs.FS = sub(legacyFiles, ".")

// Identity is user-service's migration set.
var Identity fs.FS = sub(identityFiles, "identity")

// Merchant is merchant-service's migration set.
var Merchant fs.FS = sub(merchantFiles, "merchant")

// Coupon is coupon-service's migration set.
var Coupon fs.FS = sub(couponFiles, "coupon")

// CoffeeMachine is coffee-machine-service's migration set.
var CoffeeMachine fs.FS = sub(coffeeMachineFiles, "coffee_machine")

// Order is order-service's migration set.
var Order fs.FS = sub(orderFiles, "order")

// Payment is payment-service's migration set.
var Payment fs.FS = sub(paymentFiles, "payment")

// Account is account-service's migration set.
var Account fs.FS = sub(accountFiles, "account")

// Lottery is lottery-service's migration set.
var Lottery fs.FS = sub(lotteryFiles, "lottery")

// Membership is membership-service's migration set.
var Membership fs.FS = sub(membershipFiles, "membership")

// Partner is partner-service's migration set.
var Partner fs.FS = sub(partnerFiles, "partner")

// Versions lists a set's migration file names in the order the runner applies
// them: top-level *.sql, sorted by name. It mirrors platform/database/migrate's
// own ordering so a caller can talk about "everything up to and including X"
// without restating the rule and drifting from it.
func Versions(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: read set: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// sub narrows an embedded tree to one directory. platform/database/migrate reads
// the top level of the FS it is given and does not descend, so each set must be
// handed over already rooted at its own directory.
func sub(fsys fs.FS, dir string) fs.FS {
	narrowed, err := fs.Sub(fsys, dir)
	if err != nil {
		// Both arguments are compile-time constants of the embed directives
		// above, so this can only fire if a directive and this call disagree.
		panic("migrations: embedded directory " + dir + ": " + err.Error())
	}
	return narrowed
}
