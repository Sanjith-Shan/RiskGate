package backtest

// RealisticRuleSet returns a 50-rule set shaped like one a fraud team would
// actually run, for experiment 6's benchmark and for tests: velocity limits,
// risky email domains, product codes, device patterns and risk_score
// thresholds, in the proportions of a mature set (a few allow rules, mostly
// block and review). It uses the named lists of rules.SampleLists. The
// thresholds are plausible, not tuned: this set measures evaluation speed,
// and no result about fraud caught should be read from it.
func RealisticRuleSet() string {
	return `# allow: known-good customers skip everything below
allow  if :purchaser_email_domain: in @trusted_domains and :risk_score: < 20 and :card_seconds_since_first: > 2592000
allow  if :uid_txn_count_7d: >= 5 and :risk_score: < 10
allow  if :amount: < 5 and :risk_score: < 30
allow  if :card_type: = "debit" and :card_seconds_since_first: > 7776000 and :risk_score: < 40
allow  if lower(:purchaser_email_domain:) = "icloud.com" and :device_type: = "mobile" and :risk_score: < 25

# block: model score, velocity, card testing, risky domains
block  if :risk_score: >= 90
block  if :card_txn_count_1h: >= 8
block  if :uid_txn_count_1h: >= 6
block  if :device_txn_count_1h: >= 10
block  if :distinct_cards_per_device_24h: > 5
block  if :distinct_cards_per_email_24h: > 20
block  if :purchaser_email_domain: in @risky_domains and :amount: > 300
block  if :purchaser_email_domain: = "anonymous.com" and :risk_score: >= 70
block  if :card_txn_count_1h: >= 4 and :amount: > 3 * :card_mean_amount_7d:
block  if :uid_amount_sum_24h: > 5000 and :risk_score: >= 60
block  if :card_amount_ratio_7d: > 8 and :card_txn_count_24h: >= 3
block  if :billing_region: in @blocked_regions and :risk_score: >= 50
block  if :device_info: in @suspicious_devices and :distinct_cards_per_device_24h: >= 3
block  if :product_code: = "C" and :risk_score: >= 75
block  if :product_code: = "C" and :card_network: in @prepaid_networks and :amount: > 500
block  if :email_txn_count_1h: >= 30 and :risk_score: >= 50
block  if :card_seconds_since_last: < 30 and :card_txn_count_1h: >= 3
block  if is_missing(:billing_region:) and :amount: > 1000 and :risk_score: >= 60
block  if :recipient_email_domain: in @risky_domains and :purchaser_email_domain: != :recipient_email_domain:
block  if :uid_amount_ratio_7d: > 10
block  if :device_amount_sum_1h: > 2000
block  if :card_type: = "credit" and :card_txn_count_24h: >= 15
block  if :distance: > 1000 and :risk_score: >= 65
block  if starts_with(:device_info:, "SM-") and :distinct_cards_per_device_24h: >= 4
block  if :billing_country_code: in @watched_countries and :amount: > 800

# review: worth a human look
review if :risk_score: >= 75
review if :distinct_cards_per_device_24h: > 3
review if :product_code: = "C" and is_missing(:device_info:)
review if :card_txn_count_24h: >= 10
review if :uid_txn_count_24h: >= 8
review if :amount: > 2000 and :card_seconds_since_first: < 86400
review if is_missing(:card_seconds_since_first:) and :amount: > 500
review if :purchaser_email_domain: in @risky_domains
review if :card_amount_ratio_7d: > 4
review if :device_txn_count_24h: >= 20
review if :email_amount_sum_1h: > 10000
review if :card_network: = "discover" and :risk_score: >= 50
review if :product_code: in ["H", "S"] and :amount: > 400
review if :device_type: = "mobile" and :distinct_cards_per_email_24h: > 10
review if :uid_seconds_since_last: < 60
review if not is_missing(:recipient_email_domain:) and :purchaser_email_domain: != :recipient_email_domain: and :amount: > 250
review if :billing_region: in @blocked_regions
review if :distance: > 500
review if :card_amount_sum_7d: > 15000
review if lower(:device_info:) = "windows" and :risk_score: >= 60
`
}
