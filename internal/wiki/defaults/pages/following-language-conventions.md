# Following language conventions

Every language accumulates a set of agreed shapes: how errors are phrased, how
absence is expressed, which construct is reached for when several would work.
These conventions carry no logical force — the non-conforming version compiles
and runs identically — but they are how a codebase communicates cheaply with
readers who did not write it.

The tool reports code that works but does not read the way the language reads.
The work is to adopt the conventional shape, not to restructure the logic
underneath it.

## Why this matters

Conventions are compressed communication. A reader who has internalised them
processes conforming code without conscious attention and stops only where
something is unusual. That stopping is valuable — it is how genuinely
surprising code gets noticed. Deviating for no reason spends that attention on
nothing, and worse, it desensitises the reader, so the one place you deviated
*deliberately* no longer stands out.

Some conventions also carry consequences the surface doesn't show. Phrasing
conventions for error strings exist because errors get wrapped: a capitalised
fragment reads correctly alone and wrongly in the middle of the sentence some
caller builds around it. Conventions about how absence is expressed exist
because the language's tooling and libraries behave differently for the
conventional form than for the improvised one. Neither is arbitrary taste,
even though both look like it in isolation.

And a mixed codebase costs more than a consistently unconventional one. When
two shapes both appear, every reader must decide at every site whether the
difference is meaningful. Consistency is most of the value; conforming to the
wider language's consistency is the rest.

## Techniques

**Adopt the Canonical Construct** — the language offers a purpose-built form
for what the code is doing longhand. Use it. This covers most of what the tool
reports: a switch where a chain of equality tests was written, a conversion
where a field-by-field restatement was written, a standard helper where a
hand-rolled loop was written.

**Follow the Standard Library's Shape** — when unsure what the convention is,
find the same situation in the language's own libraries and match it. That is
the reference implementation of the language's taste, and it is the shape
every reader has already seen.

**Say What You Mean About Absence** — the conventional way to express "no
value here yet" is rarely the same as the zero value that happens to compile.
Use the named, explicit form; it tells the reader the absence was intended
rather than defaulted.

**Phrase for Composition** — text that other code will embed — error strings
above all — should read correctly when wrapped inside a longer message. Write
the fragment, not the sentence: lowercase, no terminal punctuation, no
capitalised leading word unless it is a proper noun.

**Take the Tool's Own Fix** — many conventional rewrites are mechanical, and
the linter can apply them exactly. Where an automated fix exists and is
correct, prefer it: it is faster and cannot introduce a transcription error.

## Example

Before — three separate conventions missed:

```go
func classify(kind string, ctx context.Context) (Result, error) {
	if kind == "crashed" {
		return run(nil)
	} else if kind == "timed out" {
		return run(nil)
	}
	return Result{}, errors.New("Unknown kind")
}
```

The equality chain wants a tagged switch, `nil` is not how an absent context
is expressed, and the error string is capitalised, so a caller wrapping it
produces `resolve gate: Unknown kind`.

After:

```go
func classify(ctx context.Context, kind string) (Result, error) {
	switch kind {
	case "crashed", "timed out":
		return run(context.TODO())
	default:
		return Result{}, errors.New("unknown kind")
	}
}
```

Same behaviour, same branches, and now it reads the way the rest of the
language does — including the parameter order, which is its own convention.

## Techniques by language

Conventions are the one place where the language genuinely matters. Where a
rule's conventional form differs from what this page describes, that belongs
in a **language addendum** attached to this page — written when a fix has
actually gone wrong for language-specific reasons, never speculatively.

## Constraints

**A convention finding is not a correctness finding.** The code works. Do not
take the opportunity to restructure the logic, rename the surrounding
identifiers, or fix an unrelated smell you noticed while in the file. The
change should be the conventional rewrite and nothing else, so that a reviewer
can confirm it is behaviour-preserving by inspection.

**Behaviour is preserved, and tests are not edited.** These rewrites are
mechanical and should not move a single assertion. If a test fails, the
rewrite was not equivalent — revert and look again rather than adjusting the
test, which trips the test-integrity gate.

**Do not change exported names or signatures for a convention.** Parameter
order, naming style, and receiver conventions all matter, but on an exported
declaration they are an API commitment. Fix the internal cases; raise the
exported ones deliberately.

**Do not suppress a convention you disagree with.** A `//nolint` directive to
preserve a personal preference trips the suppression integrity gate and leaves
the codebase mixed, which is the outcome with the least value. If a
convention is genuinely wrong for this repository, disable the rule in the
gate configuration where the decision is visible, rather than at the site
where it is invisible.

**One rule ID, many conventions.** This page is reached by every finding from
an umbrella linter, which bundles unrelated conventions under a single rule
ID. Read the specific message; the finding names precisely which convention
was missed, and the answer for one says nothing about the others.
