# Small, composable functions

A function should do one thing, at one level of abstraction, and be readable
top to bottom without holding a stack of conditions in your head. High
cyclomatic complexity is not itself the defect — it is the symptom that
several responsibilities are sharing one body, and that the branches between
them have never been named.

The number the tool reports counts independent paths through the function.
That is a proxy, and a coarse one. The work is not to move the number below a
threshold; it is to find the responsibilities the branching is hiding and give
each one a name.

## Why this matters

A function with a dozen paths cannot be reviewed. A reader can check that each
branch is individually plausible, but not that the branches are collectively
exhaustive, and that second property is where the bugs live. Once the paths
outnumber what fits in working memory, review degrades into spot-checking.

It also cannot be tested honestly. Covering a dozen paths through one function
means constructing a dozen elaborate inputs, and the setup usually pulls in
collaborators the logic does not really need — so the tests grow mocks, and
the mocks then pin the implementation in place. Extracted pieces are usually
testable as plain functions over plain values, with no test doubles at all.

And it is where regressions cluster. The next change lands in the middle of
that thicket, interacts with a condition three branches up, and the reviewer
who could not verify exhaustiveness the first time cannot verify it now
either.

## Techniques

These are the named refactorings that resolve this finding. Pick by what the
branching is actually doing, not in list order.

**Extract Function** — the primary move. A contiguous run of statements that
serves one purpose, especially one you would write a comment above, becomes a
function named for that purpose.

**Decompose Conditional** — the condition itself is unreadable. Extract the
test into a predicate named for what it means (`isEligible(order)`), and the
branch bodies into functions named for what they do.

**Replace Nested Conditional with Guard Clauses** — the function is a pyramid.
Invert the exceptional cases into early returns so the happy path runs
unindented down the left margin. This alone often resolves the finding, and it
is usually the safest change available.

**Consolidate Conditional Expression** — several sequential conditions all
lead to the same outcome. Combine them into one predicate with a name that
says why they are the same case.

**Replace Temp with Query** — a variable is assigned differently across
several branches, forcing the branches to stay together. Replace it with a
function that computes the value, and the branches become independent.

**Introduce Parameter Object** — the branching is over combinations of loose
parameters. Group the ones that travel together into a type; the invalid
combinations often stop being representable, and their branches disappear.

**Replace Conditional with Polymorphism** — the function switches on a type or
kind tag, and the same switch appears elsewhere. Move each arm onto a type
satisfying a shared interface. Reach for this when the switch is duplicated;
for a single switch it is usually over-engineering.

**Split Loop** — one loop body does two unrelated jobs, so its complexity is
the product of both. Split into two loops, each extractable on its own. The
duplicated iteration is nearly always worth it.

## Example

Before — nested conditionals, one temp shared across branches:

```go
func Ship(order Order, stock map[string]int) (Shipment, error) {
	if order.ID != "" {
		if len(order.Lines) > 0 {
			total := 0
			for _, line := range order.Lines {
				if stock[line.SKU] >= line.Qty {
					total += line.Qty * line.UnitPrice
				} else {
					return Shipment{}, fmt.Errorf("insufficient stock for %s", line.SKU)
				}
			}
			if total > 0 {
				return Shipment{OrderID: order.ID, Total: total}, nil
			}
			return Shipment{}, errors.New("order total must be positive")
		}
		return Shipment{}, errors.New("order has no lines")
	}
	return Shipment{}, errors.New("order has no ID")
}
```

After — guard clauses hoist the error cases, and the pricing loop becomes a
query with a name:

```go
func Ship(order Order, stock map[string]int) (Shipment, error) {
	if order.ID == "" {
		return Shipment{}, errors.New("order has no ID")
	}
	if len(order.Lines) == 0 {
		return Shipment{}, errors.New("order has no lines")
	}

	total, err := totalPrice(order.Lines, stock)
	if err != nil {
		return Shipment{}, err
	}
	if total <= 0 {
		return Shipment{}, errors.New("order total must be positive")
	}
	return Shipment{OrderID: order.ID, Total: total}, nil
}

func totalPrice(lines []Line, stock map[string]int) (int, error) {
	total := 0
	for _, line := range lines {
		if stock[line.SKU] < line.Qty {
			return 0, fmt.Errorf("insufficient stock for %s", line.SKU)
		}
		total += line.Qty * line.UnitPrice
	}
	return total, nil
}
```

`totalPrice` is now testable over plain values with no `Order` and no
`Shipment`, and `Ship` reads as a list of preconditions followed by one
computation.

## Constraints

**Every extracted function must name a concept.** If the best name you can
find is `shipPart2`, `handleRest`, or `doWork`, you have cut in the wrong
place — the cut should follow a seam that already exists in the problem, not
an arbitrary line offset. Move the seam rather than accepting the name.

**Do not relocate the branching.** Moving eight paths into a helper that still
has eight paths lowers the reported number and improves nothing. The
complexity must actually decompose: each resulting function should be
independently understandable, or the refactor has failed regardless of what
the tool now says.

**Behavior is preserved, and tests are not edited to accommodate the
refactor.** No changed assertions, no deleted cases, no loosened expectations.
If a test fails after the refactor, the refactor is wrong. Changing the test
instead trips the test-integrity gate and blocks the run.

**A long but flat function is not this finding.** Straight-line code with no
branching has low complexity however many lines it runs to, and the tool will
not have flagged it. Length alone is not the target; do not restructure it on
this page's account.

**Extraction must not widen an interface.** Prefer unexported helpers in the
same file. Exporting a new symbol to satisfy a complexity finding trades a
local readability problem for a permanent API commitment.
