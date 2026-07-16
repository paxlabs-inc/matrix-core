"""AGON provider registry: a pluggable generalization of harness.MODEL_ROUTES.

The pre-port harness hard-codes a MODEL_ROUTES dict {model -> {url, key_env,
served_model, max_tokens_field, extra_body}} plus retry/backoff baked into
LLM.chat. AGON needs to run many models across several providers within each
provider's own rate limit, so the routing table becomes a first-class registry of
pluggable Provider objects, each carrying:

  - endpoint URL and the env var holding its key
  - the served-model id (per model) and the completion-budget field name
    (OpenAI split max_tokens -> max_completion_tokens)
  - extra body params (reasoning toggles, thinking blocks)
  - a per-provider concurrency limit (the work queue honors it)
  - retry/backoff policy with cost accounting
  - a tools-unsupported fallback: a provider that cannot do native tool-calls
    degrades to a documented prompt-driven fallback rather than failing.

This module is transport-agnostic: build_body() shapes the request payload and
call_with_retry() drives retry/backoff + cost accounting around any callable, so
it is fully unit-testable with a seeded fixture provider and no network.
"""

import time


class ToolsUnsupported(Exception):
    """A provider rejected native tool-calls. Callers degrade to the provider's
    documented fallback (see Provider.tools_mode)."""


class ProviderError(RuntimeError):
    """A provider call exhausted its retries."""


# Tool-call transport modes. "native" sends the OpenAI `tools` array; "prompt"
# is the documented fallback for a provider without native tool-calls: the tool
# schema is described in the prompt and calls are parsed loosely from content.
TOOLS_NATIVE = "native"
TOOLS_PROMPT = "prompt"


class Provider:
    def __init__(self, name, url, key_env, *, max_tokens_field="max_tokens",
                 extra_body=None, concurrency=2, max_retries=4, backoff_base=3.0,
                 tools_mode=TOOLS_NATIVE, retry_status=(429, 500, 502, 503, 524),
                 cost_per_mtok_in=0.0, cost_per_mtok_out=0.0):
        if concurrency < 1:
            raise ValueError("concurrency must be >= 1")
        self.name = name
        self.url = url
        self.key_env = key_env
        self.max_tokens_field = max_tokens_field
        self.extra_body = dict(extra_body or {})
        self.concurrency = int(concurrency)
        self.max_retries = int(max_retries)
        self.backoff_base = float(backoff_base)
        self.tools_mode = tools_mode
        self.retry_status = tuple(retry_status)
        self.cost_per_mtok_in = float(cost_per_mtok_in)
        self.cost_per_mtok_out = float(cost_per_mtok_out)

    @property
    def supports_tools(self):
        return self.tools_mode == TOOLS_NATIVE

    def build_body(self, served_model, messages, *, tools=None, temperature=0.3,
                   max_tokens=8192):
        """Shape the request payload for this provider. When the provider does not
        support native tools, the `tools` array is dropped (the caller supplies the
        prompt-driven fallback instead) so the request never trips a 400."""
        body = {
            "model": served_model,
            "messages": messages,
            "temperature": temperature,
            self.max_tokens_field: max_tokens,
        }
        body.update(self.extra_body)
        if tools and self.supports_tools:
            body["tools"] = tools
            body["tool_choice"] = "auto"
        return body

    def cost(self, usage):
        """USD cost of one call from its usage dict, per this provider's rate."""
        pin = (usage or {}).get("prompt_tokens", 0) or 0
        pout = (usage or {}).get("completion_tokens", 0) or 0
        return round(pin / 1_000_000 * self.cost_per_mtok_in
                     + pout / 1_000_000 * self.cost_per_mtok_out, 6)

    def call_with_retry(self, fn, *, is_tools_unsupported=None, sleep=time.sleep):
        """Drive retry/backoff around `fn` (a zero-arg callable performing one
        request and returning (payload_dict)). Returns (payload, attempts). Raises
        ToolsUnsupported (never retried) or ProviderError after exhausting retries.

        `is_tools_unsupported(exc)` classifies a raised exception as a
        tools-unsupported signal; `should_retry(exc)` classification is by the
        exception carrying a `.retryable` truthiness or a `.code` in retry_status.
        """
        last = None
        for attempt in range(self.max_retries):
            try:
                return fn(), attempt + 1
            except ToolsUnsupported:
                raise
            except Exception as e:  # noqa: BLE001 - transport errors are opaque here
                if is_tools_unsupported and is_tools_unsupported(e):
                    raise ToolsUnsupported(str(e)) from e
                last = e
                if not self._retryable(e) or attempt == self.max_retries - 1:
                    break
                sleep(self.backoff_base * (attempt + 1))
        raise ProviderError(f"{self.name}: exhausted retries: {last}")

    def _retryable(self, exc):
        code = getattr(exc, "code", None)
        if code in self.retry_status:
            return True
        # URLError / TimeoutError / JSON decode: transient transport, retry.
        return getattr(exc, "retryable", False) or code is None


class ProviderRegistry:
    """Maps a model id to its Provider and served-model id. Replaces the flat
    MODEL_ROUTES dict; a model with no explicit binding falls back to the
    registry's default provider."""

    def __init__(self, default_provider=None):
        self._providers = {}
        self._bindings = {}  # model id -> (provider_name, served_model)
        self._default = None
        if default_provider is not None:
            self.register(default_provider, default=True)

    def register(self, provider, *, default=False):
        if provider.name in self._providers:
            raise ValueError(f"duplicate provider: {provider.name}")
        self._providers[provider.name] = provider
        if default or self._default is None:
            self._default = provider.name
        return provider

    def bind(self, model, provider_name, served_model=None):
        if provider_name not in self._providers:
            raise KeyError(f"unknown provider: {provider_name}")
        self._bindings[model] = (provider_name, served_model or model)
        return self

    def provider_names(self):
        return set(self._providers)

    def provider(self, name):
        return self._providers[name]

    def provider_for(self, model):
        name = self._bindings.get(model, (self._default, None))[0]
        if name is None:
            raise KeyError(f"no provider for model {model} and no default registered")
        return self._providers[name]

    def served_model(self, model):
        binding = self._bindings.get(model)
        return binding[1] if binding else model

    def concurrency(self, name):
        return self._providers[name].concurrency


# Novita is the default catch-all endpoint (mirrors harness.DEFAULT_ROUTE).
NOVITA_URL = "https://api.novita.ai/openai/v1/chat/completions"


def default_registry():
    """Build the registry mirroring harness.DEFAULT_ROUTE + MODEL_ROUTES so the
    generalization is behavior-preserving for the models dojo already routes."""
    reg = ProviderRegistry(default_provider=Provider(
        "novita", NOVITA_URL, "NOVITA_API_KEY", concurrency=4))
    reg.register(Provider(
        "xiaomi", "https://api.xiaomimimo.com/v1/chat/completions", "XIAOMI_API_KEY",
        max_tokens_field="max_completion_tokens", concurrency=2))
    reg.register(Provider(
        "deepseek", "https://api.deepseek.com/chat/completions", "DEEPSEEK_API",
        extra_body={"reasoning_effort": "high", "thinking": {"type": "enabled"}},
        concurrency=2))
    reg.bind("xiaomimimo/mimo-v2.5-pro-ultraspeed-direct", "xiaomi", "mimo-v2.5-pro-ultraspeed")
    reg.bind("deepseek/deepseek-v4-pro-direct", "deepseek", "deepseek-v4-pro")
    return reg
