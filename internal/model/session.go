package model

// Session chat memory never uses the shared code Node label or FACT edges.
const (
	KindSessionMemoryEntity NodeKind = "SessionMemoryEntity"
	EdgeSessionFact         EdgeKind = "SESSION_FACT"
)
