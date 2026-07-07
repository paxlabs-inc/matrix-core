import threading
import time
import unittest

from agon.provider import (
    Provider,
    ProviderError,
    ProviderRegistry,
    ToolsUnsupported,
    TOOLS_PROMPT,
    default_registry,
)
from agon.workqueue import build_units, run_queue, run_serial


def _registry():
    reg = ProviderRegistry(default_provider=Provider("fast", "http://x", "K", concurrency=3))
    reg.register(Provider("slow", "http://y", "K2", concurrency=1))
    reg.bind("m_fast_a", "fast")
    reg.bind("m_fast_b", "fast")
    reg.bind("m_slow", "slow")
    return reg


def _scenarios():
    return [{"id": f"s{i}", "kind": "agentic"} for i in range(6)]


def _score(unit):
    # Deterministic pure function of the unit identity - no I/O, no randomness.
    return {"model": unit.model, "scenario": unit.scenario["id"], "rep": unit.rep,
            "value": (hash((unit.model, unit.scenario["id"], unit.rep)) % 1000) / 1000.0}


class TestWorkQueue(unittest.TestCase):
    def test_queue_matches_serial(self):
        reg = _registry()
        units = build_units(["m_fast_a", "m_slow", "m_fast_b"], _scenarios(), 2, reg)
        serial = run_serial(units, _score)
        concurrent = run_queue(units, _score, reg, global_concurrency=8)
        self.assertEqual(serial, concurrent)

    def test_output_order_is_deterministic(self):
        reg = _registry()
        units = build_units(["m_fast_a", "m_slow"], _scenarios(), 1, reg)
        # Even with jittered per-unit delay, output stays in unit order.
        def slow_worker(unit):
            time.sleep(0.002 * (unit.index % 3))
            return unit.index
        out = run_queue(units, slow_worker, reg, global_concurrency=8)
        self.assertEqual(out, list(range(len(units))))

    def test_per_provider_concurrency_honored(self):
        reg = _registry()  # fast=3, slow=1
        live = {"fast": 0, "slow": 0}
        peak = {"fast": 0, "slow": 0}
        lock = threading.Lock()

        def worker(unit):
            with lock:
                live[unit.provider_name] += 1
                peak[unit.provider_name] = max(peak[unit.provider_name], live[unit.provider_name])
            time.sleep(0.01)
            with lock:
                live[unit.provider_name] -= 1
            return unit.index

        units = build_units(["m_fast_a", "m_fast_b", "m_slow"],
                            [{"id": f"s{i}", "kind": "agentic"} for i in range(8)], 1, reg)
        run_queue(units, worker, reg, global_concurrency=16)
        self.assertLessEqual(peak["fast"], 3)
        self.assertLessEqual(peak["slow"], 1)
        # And the fast provider actually reached parallelism (proves it isn't serial).
        self.assertGreaterEqual(peak["fast"], 2)

    def test_empty_units(self):
        self.assertEqual(run_queue([], _score, _registry()), [])


class TestProviderRegistry(unittest.TestCase):
    def test_default_registry_mirrors_routes(self):
        reg = default_registry()
        # Unknown model -> default provider (Novita), served id unchanged.
        p = reg.provider_for("some/unknown-model")
        self.assertEqual(p.name, "novita")
        self.assertEqual(reg.served_model("some/unknown-model"), "some/unknown-model")
        # Bound models map to their provider + served id.
        self.assertEqual(reg.provider_for("deepseek/deepseek-v4-pro-direct").name, "deepseek")
        self.assertEqual(reg.served_model("deepseek/deepseek-v4-pro-direct"), "deepseek-v4-pro")
        self.assertEqual(reg.served_model("xiaomimimo/mimo-v2.5-pro-direct"), "mimo-v2.5-pro")

    def test_build_body_max_tokens_field_and_extra(self):
        ds = default_registry().provider("deepseek")
        body = ds.build_body("deepseek-v4-pro", [{"role": "user", "content": "hi"}],
                             tools=[{"type": "function"}], max_tokens=4096)
        self.assertIn("max_tokens", body)
        self.assertEqual(body["max_tokens"], 4096)
        self.assertEqual(body["reasoning_effort"], "high")
        self.assertIn("tools", body)
        xi = default_registry().provider("xiaomi")
        body2 = xi.build_body("mimo-v2.5-pro", [], max_tokens=8192)
        self.assertIn("max_completion_tokens", body2)
        self.assertNotIn("max_tokens", body2)

    def test_tools_unsupported_fallback_drops_tools(self):
        p = Provider("noant", "http://z", "K", tools_mode=TOOLS_PROMPT)
        self.assertFalse(p.supports_tools)
        body = p.build_body("m", [], tools=[{"type": "function"}])
        self.assertNotIn("tools", body)  # fallback: never sends a tools array

    def test_cost_accounting(self):
        p = Provider("priced", "http://z", "K", cost_per_mtok_in=0.5, cost_per_mtok_out=1.5)
        cost = p.cost({"prompt_tokens": 1_000_000, "completion_tokens": 2_000_000})
        self.assertAlmostEqual(cost, 0.5 + 3.0, places=6)

    def test_retry_backoff_then_success(self):
        p = Provider("r", "http://z", "K", max_retries=4, backoff_base=0.0)
        slept = []
        state = {"n": 0}

        class Err(Exception):
            code = 503

        def fn():
            state["n"] += 1
            if state["n"] < 3:
                raise Err()
            return {"ok": True}

        payload, attempts = p.call_with_retry(fn, sleep=lambda s: slept.append(s))
        self.assertEqual(payload, {"ok": True})
        self.assertEqual(attempts, 3)
        self.assertEqual(len(slept), 2)  # slept before each of the 2 retries

    def test_retry_exhausted_raises(self):
        p = Provider("r", "http://z", "K", max_retries=2, backoff_base=0.0)

        class Err(Exception):
            code = 500

        def fn():
            raise Err()

        with self.assertRaises(ProviderError):
            p.call_with_retry(fn, sleep=lambda s: None)

    def test_tools_unsupported_never_retried(self):
        p = Provider("r", "http://z", "K", max_retries=4, backoff_base=0.0)
        calls = {"n": 0}

        def fn():
            calls["n"] += 1
            raise ValueError("no function calling")

        with self.assertRaises(ToolsUnsupported):
            p.call_with_retry(fn, is_tools_unsupported=lambda e: "function" in str(e),
                              sleep=lambda s: None)
        self.assertEqual(calls["n"], 1)  # classified immediately, not retried


if __name__ == "__main__":
    unittest.main()
