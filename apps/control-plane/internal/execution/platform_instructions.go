package execution

// platformInstructions is §25.12 layer 1's control-plane half: the working discipline every run
// carries, authored by the platform and narrowable by a revision (layer 3) or a run (layer 5)
// speaking after it. The engine supplies the other half — its own identity and protocol rules — and
// keeps doing so; a second engine would carry a different one, which is exactly why discipline that
// must hold for every engine lives here instead of there.
//
// WHAT THIS REPLACED, because the gap is the reason it exists: for a long time the only thing a
// model was told was the engine's twenty-seven words, all of them protocol ("never control lifecycle
// state"). Nothing said how to explore, when to verify, or what not to build.
//
// PROVENANCE, because this is the kind of text whose origin gets asked about. The patterns here are
// drawn from Anthropic's public engineering writing on agent and tool design and from the
// MIT-licensed anthropics/claude-code-action; the wording is ours. Nothing is copied from Claude
// Code's system prompt, which Anthropic never published — it escaped through an npm packaging error
// in March 2026 and Anthropic filed 8,100+ DMCA notices over it. Patterns are not copyrightable;
// text is.
//
// IT NAMES NO TOOL, and a guard enforces that. A tool's description belongs on the tool, where the
// advertisement seam already carries it to the model; restating one here would create a second copy
// to drift. The text may say what the environment AFFORDS, never which tool affords it.
//
// AND IT PRESCRIBES NO CHARACTER — the correction that produced this version, and the more important
// of the two rules. The first draft opened "Work like a careful colleague" and went on to instruct
// restraint, verification habits and a reporting style. That is the layer-3 author's sentence to
// write, not the platform's: a deployment building a deliberately bold agent would have found layer 1
// arguing with it inside the same prompt, and the model splitting the difference between two voices
// neither operator chose. A layer that every revision inherits and none asked for can only describe
// the WORLD — what exists, what it costs, how it answers. Who the agent IS belongs to whoever
// configures it.
//
// The line between the two is not style, it is falsifiability: "a change must match exactly once" is
// a fact about the mechanism and stays true under every persona; "change only the lines the task
// needs" is a preference, and a preference stated here overrides one stated by the operator.
const platformInstructions = `How work reaches you here, so you can plan around it rather than discover it.

Looking things up. Targeted search over names and over file contents is available, and reading a bounded range of a file is available; neither requires pulling a whole file to find out what is in it. A search that matches nothing has told you the pattern does not occur — which is a result about the pattern, not yet about the thing you were looking for.

Several at once. Calls that do not depend on each other can be requested in the same turn, and their results come back together. A turn is the unit that costs time here: two turns cost roughly twice one, whatever the calls inside them were, and a call whose answer nothing is waiting on has cost you a turn for nothing.

Changing files. Targeted replacement of a passage is available, so rewriting a file end to end is not the only way to edit it. A replacement has to match exactly one place in the file: zero matches and several matches are both refused, because neither says which text was meant.

Where changes go. Edits accumulate in a workspace and become a reviewable set. Getting them anywhere beyond that workspace is a separate step, and some deployments route that step through a human.

Running things. Commands can be run and their output comes back to you, including the exit status. Work that outlasts a single call can run in the background and be checked later.

When something refuses. A refusal is an answer, and it carries its reason. An identical call gets an identical answer, so a different outcome needs a different call. Some calls wait on a human decision; the run holds there and resumes with the answer, and nothing is lost while it waits.`
