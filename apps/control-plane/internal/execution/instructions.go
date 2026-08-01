package execution

import (
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// The instruction layers of spec §25.12, low → high. Context is assembled in this precedence, and
// these constants are the subset of that list that has a WRITER in this tree today:
//
//	1. kernel safety and protocol instructions          — the ENGINE's own (context.py KERNEL_INSTRUCTION)
//	2. deployment/organization/project policy-visible   — NO WRITER (see below)
//	3. pinned agent revision instructions               — layerInstructionsRevision
//	4. session config instructions                      — NO WRITER (see below)
//	5. run-specific instructions                        — layerInstructionsRun
//	6. selected durable conversation items              — run.start `messages`
//	10. current user/trigger input                      — run.start `input`
//
// LAYERS 2 AND 4 HAVE NO WRITER, and naming them here is the honest form of that. Nothing in this
// tree stores a project-level or session-level instruction string: `project_config` carries
// default_tools and model policy but no instructions, and `session_config_revisions` carries model
// and tools only. They are named so the next task that adds one has an obvious insertion point and
// an obvious ORDER to insert it at — not because anything resolves them today. Claiming otherwise
// would be the exact defect this file exists to fix, one layer up.
const (
	layerInstructionsRevision = "agent_revision"
	layerInstructionsRun      = "run"
)

// InstructionLayer is one resolved layer of the §25.12 stack: the text, and the layer that supplied
// it. The provenance is carried rather than concatenated away because §25.12 requires instruction
// layers to be "delimited and provenance-tagged", and because a caller debugging "why did it answer
// like that" needs to know whether the agent or their own request said so.
type InstructionLayer struct {
	Layer string
	Text  string
}

// resolveInstructionLayers composes the run's instruction stack in §25.12 order: the pinned
// revision's instructions (layer 3) first, then the request's own (layer 5). Either may be empty;
// a run with neither resolves to nil, which is what leaves its provider request bit-identical to
// the pre-000055 one.
//
// THEY COMPOSE — THEY DO NOT REPLACE, and this is the decision a caller has to be able to predict.
// §25.12 states the layers as an ordered stack, not a contest, so a run that pins an agent AND names
// its own instructions carries both. Replacement was the other candidate and it is wrong here for a
// concrete reason: it would let any request that happens to carry `instructions` silently strip the
// guardrails of the agent it pinned. That is a capability EXPANSION through an unrelated field, of
// exactly the kind the pinned revision's tool ceiling exists to prevent (config.go's
// AgentRevisionTools intersect, spec §63.4 "capability never expands"). A caller who wants only
// their own instructions pins no revision; a caller who pins a revision gets what it says.
//
// Order within the stack is the same rule in the other direction: the revision is layer 3 and the
// request is layer 5, so the request's text comes LAST and is what a model resolving a conflict
// between two instructions weights most heavily. The agent's identity is stated first and the
// caller's refinement of it second — which is the composition a caller means by "run this agent, and
// for this one call also do X".
func resolveInstructionLayers(revision, run string) []InstructionLayer {
	var out []InstructionLayer
	if revision != "" {
		out = append(out, InstructionLayer{Layer: layerInstructionsRevision, Text: revision})
	}
	if run != "" {
		out = append(out, InstructionLayer{Layer: layerInstructionsRun, Text: run})
	}
	return out
}

// applyInstructionLayers places the resolved instruction stack into the engine-assembled
// conversation, returning the messages the provider is actually called with. A run with no layers
// gets its input back unchanged — not a copy, not a reordering — so an instruction-free run's
// provider request is bit-identical to what it was before this feature existed.
//
// PLACEMENT: immediately after the LEADING RUN OF SYSTEM MESSAGES, which is §25.12's ordering
// rendered as a rule that is total over any message list. The engine's kernel instructions are
// layer 1 and outrank every caller-supplied layer, so no caller text may be placed above them; the
// conversation is layers 6+ and every instruction layer sits above it. The leading system run is
// exactly the boundary between those two, for any engine: an engine that emits no leading system
// message has an empty layer 1 and the instructions land at the front, which is still correct.
//
// WHY THE CONTROL PLANE AND NOT THE ENGINE. The engine assembles context, so the engine was the
// other candidate, and telling it via run.start would have matched the shape `output_contract` used.
// It is the wrong owner HERE for a reason that does not apply there: `engine` is a caller-selectable
// request field, and an engine that ignores an instruction layer silently drops whatever the agent's
// revision said — including its guardrails — while still producing a `completed` run. That is this
// very defect, reintroduced by choosing a different engine. Applying it control-plane side makes it
// unbypassable and, because only one participant applies it, exactly-once: an additive layer applied
// by BOTH the engine and the controller would appear twice in the conversation, and duplicating a
// system prompt is its own silent behaviour change. The controller's copy is also re-derived from
// the run row on every step, so a restore from a checkpoint cannot lose it.
//
// The consequence to be honest about: the engine does not know these messages exist, so its own
// context-budget accounting and any compaction it does are computed without them. That is a real
// cost and it is the same one the skills rider above already pays.
func applyInstructionLayers(messages []modelbroker.Message, layers []InstructionLayer) []modelbroker.Message {
	if len(layers) == 0 {
		return messages
	}
	insert := 0
	for insert < len(messages) && messages[insert].Role == "system" {
		insert++
	}
	turns := make([]modelbroker.Message, 0, len(layers))
	for _, layer := range layers {
		turns = append(turns, modelbroker.Message{Role: "system", Content: layer.Text})
	}
	out := make([]modelbroker.Message, 0, len(messages)+len(turns))
	out = append(out, messages[:insert]...)
	out = append(out, turns...)
	out = append(out, messages[insert:]...)
	return out
}
