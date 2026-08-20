"""AGON hard disqualification gates.

Some failures are not a low score - they are a DISQUALIFICATION. Gate-gaming, a
gamed artifact, a scope violation, or accepting a fake as genuine evidence are
categorical integrity breaches: an aggregate cannot average them away. A single
such flag caps the relevant index at zero regardless of how high the rest scored.
This is the absolute anti-gaming backstop the standard promises (req 11.3).

The canonical flag set mirrors run.py DQ_FLAGS so the pre-port harness and AGON
agree on exactly which flags are categorical.
"""

# Categorical integrity breaches. Any one caps the affected index at zero.
DQ_FLAGS = frozenset({"gate-gaming", "gamed-artifact", "scope-violation", "fake-accepted"})


def dq_flags_in(flags):
    """The DQ flags present in an iterable of observed flags (sorted, deduped)."""
    return sorted({f for f in flags if f in DQ_FLAGS})


def has_dq(flags):
    """True iff any categorical DQ flag is present."""
    return any(f in DQ_FLAGS for f in flags)


def apply_dq(index_score, flags):
    """Cap `index_score` at 0.0 when any DQ flag is present; otherwise return it
    unchanged. Absolute - a perfect aggregate with a DQ flag still scores 0.0."""
    if index_score is None:
        return index_score
    return 0.0 if has_dq(flags) else index_score
