---
layout: layouts/blog-post.njk
title: "Understanding Intent IR: Why Typed Intents Matter"
date: 2025-01-10
author: "Engineering Team"
tags: ["technical", "architecture"]
excerpt: "A deep dive into Matrix's Intent IR — how we preserve meaning through multi-step execution by typing intent at the compiler level."
---

One of the core innovations in Matrix is the Intent IR — a typed intermediate representation that preserves meaning through every stage of execution.

## Why Not Just Use Prompts?

Traditional agent systems pass natural language directly to LLMs at each step. This means meaning must be re-inferred at every context boundary. The result: intent drift, hallucinated actions, and unpredictable behavior.

## The Typed Approach

Matrix's MCL compiler transforms prose into a formal IR with:

- **Verb nodes**: One of 10 canonical actions (find, acquire, build, modify, deliver, analyze, negotiate, schedule, monitor, delegate)
- **Kind nodes**: One of 8 typed operands (service, model, agent, knowledge, intent, asset, plan, capability)
- **Constraint edges**: Typed relationships between nodes

## Canonical Hashing

Two semantically identical inputs always produce the same AST hash. Whitespace and comments are normalized away. This enables content-addressed caching and deduplication of equivalent intents.

## What This Means for Developers

When you compile an intent, you get a deterministic, inspectable artifact. You can hash it, compare it, cache it, and replay it. No more "it worked yesterday but not today."
