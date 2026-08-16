# Keith portable export format

`keith-portable-export` is a standalone JSON document. Schema version 1 contains an exact profile/session scope, one data domain, versioned transactional records, and files represented by relative paths, SHA-256 digests, and hexadecimal bytes. It can be inspected or decoded with any JSON and hexadecimal tooling; Keith does not need to be installed.

Each domain is exported separately. Session, workspace, memory, knowledge, skill, artifact, and credential exports contain files. Schedule, commitment, route, channel-state, and tool-experience exports contain transactional records. Credentials remain opaque encrypted bytes.

Restore creates missing files and records without overwriting existing state. Deletion requires the confirmation digest from an exact deletion plan and reports every target that remains after a partial failure.
