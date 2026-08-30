package rules_test

import (
	"fmt"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Load runs the whole pipeline: parse, type-check against the catalog and
// lists, lint, and compile to closures. The rule set then decides payments.
func ExampleLoad() {
	env := rules.Env{
		Catalog: schema.Default(),
		Lists:   rules.Lists{"trusted_domains": rules.StringList("gmail.com", "icloud.com")},
	}
	src := `
allow  if :purchaser_email_domain: in @trusted_domains
block  if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:
review if :product_code: = "C" and is_missing(:device_info:)
shadow block if :amount: > 1000
`
	rs, err := rules.Load(src, env, 42)
	if err != nil {
		fmt.Println(err)
		return
	}

	cat := env.Catalog
	payment := cat.NewRow() // every field starts missing
	payment.Num[cat.MustLookup("amount").Slot] = 1200
	payment.Num[cat.MustLookup("card_txn_count_1h").Slot] = 9
	payment.Num[cat.MustLookup("card_mean_amount_7d").Slot] = 40

	d := rs.Evaluate(payment)
	fmt.Printf("%s by rule %d (%s), rule set v%d\n", d.Action, d.Rule.Index, d.Rule.Text, d.Version)
	for _, s := range d.Shadow {
		fmt.Printf("shadow match: %s\n", s.Text)
	}
	// Output:
	// block by rule 1 (block  if :card_txn_count_1h: >= 8 and :amount: > 3 * :card_mean_amount_7d:), rule set v42
	// shadow match: shadow block if :amount: > 1000
}

// Errors come back as Diagnostics, rendered for the person who wrote the rule.
func ExampleLoad_errors() {
	env := rules.Env{Catalog: schema.Default()}
	_, err := rules.Load(`block if :card_txn_cnt_1h: >= 8`, env, 1)
	if ds, ok := err.(rules.Diagnostics); ok {
		fmt.Println(ds.Render())
	}
	// Output:
	// error at line 1, column 10:
	// block if :card_txn_cnt_1h: >= 8
	//          ^^^^^^^^^^^^^^^^^
	// unknown attribute :card_txn_cnt_1h:. Did you mean :card_txn_count_1h:?
}

// Print gives canonical text with only the parentheses precedence needs.
func ExamplePrint() {
	e, _ := rules.ParseExpr(`(:amount: > 100 or (:risk_score: > 80 and not (is_missing(:device_info:))))`)
	fmt.Println(rules.Print(e))
	// Output:
	// :amount: > 100 or :risk_score: > 80 and not is_missing(:device_info:)
}
