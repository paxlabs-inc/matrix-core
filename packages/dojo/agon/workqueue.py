"""AGON bounded-concurrency work queue.

The pre-port runner (run.py main()) executes (model x scenario x rep) units in a
fully serial triple loop, so a full gauntlet is hours. AGON runs the same units
through a bounded-concurrency work queue that honors BOTH a global worker cap and
each PROVIDER's own concurrency limit (a provider's rate limit must never be
exceeded no matter how wide the global pool is).

The queue is deterministic in its output: results are returned in the original
unit order regardless of completion order, so a concurrent run scores identically
to a serial run on the same inputs. Isolation of work is by unit; the worker
callable owns all model I/O and scoring.
"""

import threading
from concurrent.futures import ThreadPoolExecutor


class WorkUnit:
    """One (model, scenario, rep) unit of work. `index` fixes the deterministic
    output order; `provider_name` selects the per-provider concurrency gate."""

    __slots__ = ("index", "model", "scenario", "rep", "provider_name", "kind")

    def __init__(self, index, model, scenario, rep, provider_name, kind):
        self.index = index
        self.model = model
        self.scenario = scenario
        self.rep = rep
        self.provider_name = provider_name
        self.kind = kind


def build_units(models, scenarios, reps, registry):
    """Flatten models x scenarios x reps into ordered WorkUnits, tagging each with
    its provider (from the registry) so the queue can gate per provider."""
    units = []
    idx = 0
    for model in models:
        pname = registry.provider_for(model).name
        for scen in scenarios:
            for rep in range(reps):
                units.append(WorkUnit(idx, model, scen, rep, pname,
                                      scen.get("kind", "agentic")))
                idx += 1
    return units


class _ProviderGate:
    """Per-provider semaphores sized from the registry. Acquiring a provider's
    slot blocks once its concurrency limit is saturated."""

    def __init__(self, registry):
        self._sems = {name: threading.Semaphore(registry.concurrency(name))
                      for name in registry.provider_names()}

    def semaphore(self, name):
        return self._sems[name]


def run_queue(units, worker, registry, *, global_concurrency=8):
    """Execute `units` through the work queue. `worker(unit) -> result` performs
    all model I/O and scoring for one unit. Returns results ordered by unit.index
    (deterministic, matches a serial run). Per-provider concurrency and the global
    worker cap are both honored."""
    gate = _ProviderGate(registry)
    results = [None] * len(units)

    def task(unit):
        sem = gate.semaphore(unit.provider_name)
        sem.acquire()
        try:
            return unit.index, worker(unit)
        finally:
            sem.release()

    if not units:
        return results
    workers = max(1, min(global_concurrency, len(units)))
    with ThreadPoolExecutor(max_workers=workers) as ex:
        for idx, res in ex.map(task, units):
            results[idx] = res
    return results


def run_serial(units, worker):
    """Reference serial execution (for parity tests and no-thread environments)."""
    return [worker(unit) for unit in units]
