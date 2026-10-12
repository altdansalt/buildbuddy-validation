# Findings

One finding per file: `<component>-<area>-<short-slug>.md`. Use
[TEMPLATE.md](TEMPLATE.md) and keep its metadata labels intact. Components are
`app`, `executor`, `cacheproxy`, or a slash-separated combination. Severity is
`critical`, `high`, `medium`, or `low`, with an impact-based justification.
Status is `unverified`, `confirmed`, or `rejected`.

A confirmed finding needs a bug-tagged repro target, exact released image
version/digest, expected vs. actual behavior, relevant logs, two reproductions,
and BuildBuddy invocation links. Rule out harness/configuration mistakes first.
A rejected finding should explain why; retain the record to avoid duplicate work.
Never file upstream issues or PRs.

[INDEX.md](INDEX.md) is generated. Run `tools/findings_index` from any directory
(or `bb remote run //tools:findings_index`); never edit INDEX.md by hand.
`tools/findings_index --check` exits nonzero if the index is stale.
