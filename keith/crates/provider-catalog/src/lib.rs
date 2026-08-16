#![forbid(unsafe_code)]

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProviderTransport {
    OpenAiChat,
    AnthropicMessages,
    GoogleGenerativeAi,
    AzureOpenAi,
    AmazonBedrock,
}

impl ProviderTransport {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OpenAiChat => "openai_chat_completions",
            Self::AnthropicMessages => "anthropic_messages",
            Self::GoogleGenerativeAi => "google_generative_ai",
            Self::AzureOpenAi => "azure_openai",
            Self::AmazonBedrock => "amazon_bedrock",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProviderAuthentication {
    BearerToken,
    ApiKeyHeader,
    GoogleApiKey,
    CloudflareApiToken,
    OAuth,
    AwsCredentials,
}

impl ProviderAuthentication {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::BearerToken => "bearer_token",
            Self::ApiKeyHeader => "api_key",
            Self::GoogleApiKey => "google_api_key",
            Self::CloudflareApiToken => "cloudflare_api_token",
            Self::OAuth => "oauth",
            Self::AwsCredentials => "aws_credentials",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderSpec {
    pub id: &'static str,
    pub display_name: &'static str,
    pub transport: ProviderTransport,
    pub authentication: ProviderAuthentication,
    pub credential_environment: &'static str,
    pub default_base_url: Option<&'static str>,
    pub default_model: &'static str,
}

macro_rules! provider {
    ($id:literal, $name:literal, $transport:ident, $auth:ident, $env:literal, $url:expr, $model:literal) => {
        ProviderSpec {
            id: $id,
            display_name: $name,
            transport: ProviderTransport::$transport,
            authentication: ProviderAuthentication::$auth,
            credential_environment: $env,
            default_base_url: $url,
            default_model: $model,
        }
    };
}

pub const BUILTIN_PROVIDERS: &[ProviderSpec] = &[
    provider!(
        "openai",
        "OpenAI",
        OpenAiChat,
        BearerToken,
        "OPENAI_API_KEY",
        Some("https://api.openai.com/v1"),
        "gpt-4.1-mini"
    ),
    provider!(
        "anthropic",
        "Anthropic",
        AnthropicMessages,
        ApiKeyHeader,
        "ANTHROPIC_API_KEY",
        Some("https://api.anthropic.com"),
        "claude-sonnet-4-5"
    ),
    provider!(
        "openai-codex",
        "ChatGPT Plus/Pro (Codex)",
        OpenAiChat,
        OAuth,
        "",
        Some("https://chatgpt.com/backend-api"),
        "gpt-5.1"
    ),
    provider!(
        "github-copilot",
        "GitHub Copilot",
        AnthropicMessages,
        OAuth,
        "GITHUB_TOKEN",
        Some("https://api.individual.githubcopilot.com"),
        "claude-sonnet-4-5"
    ),
    provider!(
        "prime-inference",
        "Prime Inference",
        OpenAiChat,
        BearerToken,
        "PRIME_API_KEY",
        Some("https://api.pinference.ai/api/v1"),
        "anthropic/claude-sonnet-4-5"
    ),
    provider!(
        "azure-openai-responses",
        "Azure OpenAI",
        AzureOpenAi,
        ApiKeyHeader,
        "AZURE_OPENAI_API_KEY",
        None,
        "gpt-4.1"
    ),
    provider!(
        "amazon-bedrock",
        "Amazon Bedrock",
        AmazonBedrock,
        BearerToken,
        "AWS_BEARER_TOKEN_BEDROCK",
        Some("https://bedrock-runtime.us-east-1.amazonaws.com"),
        "us.anthropic.claude-sonnet-4-20250514-v1:0"
    ),
    provider!(
        "google",
        "Google Gemini",
        GoogleGenerativeAi,
        GoogleApiKey,
        "GEMINI_API_KEY",
        Some("https://generativelanguage.googleapis.com/v1beta/openai"),
        "gemini-2.5-flash"
    ),
    provider!(
        "google-vertex",
        "Google Vertex AI",
        GoogleGenerativeAi,
        OAuth,
        "GOOGLE_CLOUD_API_KEY",
        None,
        "gemini-2.5-flash"
    ),
    provider!(
        "deepseek",
        "DeepSeek",
        OpenAiChat,
        BearerToken,
        "DEEPSEEK_API_KEY",
        Some("https://api.deepseek.com"),
        "deepseek-chat"
    ),
    provider!(
        "mistral",
        "Mistral",
        OpenAiChat,
        BearerToken,
        "MISTRAL_API_KEY",
        Some("https://api.mistral.ai/v1"),
        "mistral-large-latest"
    ),
    provider!(
        "groq",
        "Groq",
        OpenAiChat,
        BearerToken,
        "GROQ_API_KEY",
        Some("https://api.groq.com/openai/v1"),
        "llama-3.3-70b-versatile"
    ),
    provider!(
        "cerebras",
        "Cerebras",
        OpenAiChat,
        BearerToken,
        "CEREBRAS_API_KEY",
        Some("https://api.cerebras.ai/v1"),
        "llama-3.3-70b"
    ),
    provider!(
        "xai",
        "xAI",
        OpenAiChat,
        BearerToken,
        "XAI_API_KEY",
        Some("https://api.x.ai/v1"),
        "grok-4"
    ),
    provider!(
        "openrouter",
        "OpenRouter",
        OpenAiChat,
        BearerToken,
        "OPENROUTER_API_KEY",
        Some("https://openrouter.ai/api/v1"),
        "anthropic/claude-sonnet-4"
    ),
    provider!(
        "vercel-ai-gateway",
        "Vercel AI Gateway",
        AnthropicMessages,
        ApiKeyHeader,
        "AI_GATEWAY_API_KEY",
        Some("https://ai-gateway.vercel.sh"),
        "anthropic/claude-sonnet-4"
    ),
    provider!(
        "zai",
        "Z.AI",
        OpenAiChat,
        BearerToken,
        "ZAI_API_KEY",
        Some("https://api.z.ai/api/coding/paas/v4"),
        "glm-4.7"
    ),
    provider!(
        "opencode",
        "OpenCode Zen",
        OpenAiChat,
        BearerToken,
        "OPENCODE_API_KEY",
        Some("https://opencode.ai/zen/v1"),
        "big-pickle"
    ),
    provider!(
        "opencode-go",
        "OpenCode Go",
        OpenAiChat,
        BearerToken,
        "OPENCODE_API_KEY",
        Some("https://opencode.ai/zen/go/v1"),
        "deepseek-chat"
    ),
    provider!(
        "huggingface",
        "Hugging Face",
        OpenAiChat,
        BearerToken,
        "HF_TOKEN",
        Some("https://router.huggingface.co/v1"),
        "deepseek-ai/DeepSeek-V3.2"
    ),
    provider!(
        "fireworks",
        "Fireworks",
        AnthropicMessages,
        ApiKeyHeader,
        "FIREWORKS_API_KEY",
        Some("https://api.fireworks.ai/inference"),
        "accounts/fireworks/models/deepseek-v3p1"
    ),
    provider!(
        "kimi-coding",
        "Kimi For Coding",
        AnthropicMessages,
        ApiKeyHeader,
        "KIMI_API_KEY",
        Some("https://api.kimi.com/coding"),
        "k2p5"
    ),
    provider!(
        "minimax",
        "MiniMax",
        AnthropicMessages,
        ApiKeyHeader,
        "MINIMAX_API_KEY",
        Some("https://api.minimax.io/anthropic"),
        "MiniMax-M2.7"
    ),
    provider!(
        "minimax-cn",
        "MiniMax China",
        AnthropicMessages,
        ApiKeyHeader,
        "MINIMAX_CN_API_KEY",
        Some("https://api.minimaxi.com/anthropic"),
        "MiniMax-M2.7"
    ),
    provider!(
        "moonshotai",
        "Moonshot AI",
        OpenAiChat,
        BearerToken,
        "MOONSHOT_API_KEY",
        Some("https://api.moonshot.ai/v1"),
        "kimi-k2.5"
    ),
    provider!(
        "moonshotai-cn",
        "Moonshot AI China",
        OpenAiChat,
        BearerToken,
        "MOONSHOT_API_KEY",
        Some("https://api.moonshot.cn/v1"),
        "kimi-k2.5"
    ),
    provider!(
        "xiaomi",
        "Xiaomi MiMo",
        AnthropicMessages,
        ApiKeyHeader,
        "XIAOMI_API_KEY",
        Some("https://api.xiaomimimo.com/anthropic"),
        "mimo-v2.5-pro"
    ),
    provider!(
        "xiaomi-token-plan-cn",
        "Xiaomi MiMo Token Plan China",
        AnthropicMessages,
        ApiKeyHeader,
        "XIAOMI_TOKEN_PLAN_CN_API_KEY",
        Some("https://token-plan-cn.xiaomimimo.com/anthropic"),
        "mimo-v2-flash"
    ),
    provider!(
        "xiaomi-token-plan-ams",
        "Xiaomi MiMo Token Plan Amsterdam",
        AnthropicMessages,
        ApiKeyHeader,
        "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
        Some("https://token-plan-ams.xiaomimimo.com/anthropic"),
        "mimo-v2-flash"
    ),
    provider!(
        "xiaomi-token-plan-sgp",
        "Xiaomi MiMo Token Plan Singapore",
        AnthropicMessages,
        ApiKeyHeader,
        "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
        Some("https://token-plan-sgp.xiaomimimo.com/anthropic"),
        "mimo-v2-flash"
    ),
    provider!(
        "cloudflare-ai-gateway",
        "Cloudflare AI Gateway",
        AnthropicMessages,
        CloudflareApiToken,
        "CLOUDFLARE_API_KEY",
        None,
        "claude-sonnet-4-5"
    ),
    provider!(
        "cloudflare-workers-ai",
        "Cloudflare Workers AI",
        OpenAiChat,
        CloudflareApiToken,
        "CLOUDFLARE_API_KEY",
        None,
        "@cf/meta/llama-3.3-70b-instruct-fp8-fast"
    ),
    provider!(
        "dashscope",
        "Alibaba DashScope (Qwen)",
        OpenAiChat,
        BearerToken,
        "DASHSCOPE_API_KEY",
        Some("https://dashscope.aliyuncs.com/compatible-mode/v1"),
        "qwen3-max"
    ),
    provider!(
        "qianfan",
        "Baidu Qianfan (ERNIE)",
        OpenAiChat,
        BearerToken,
        "QIANFAN_API_KEY",
        Some("https://qianfan.baidubce.com/v2"),
        "ernie-5.0"
    ),
    provider!(
        "zhipu",
        "Zhipu AI (GLM)",
        OpenAiChat,
        BearerToken,
        "ZHIPU_API_KEY",
        Some("https://open.bigmodel.cn/api/paas/v4"),
        "glm-4.7"
    ),
    provider!(
        "modelscope",
        "ModelScope",
        OpenAiChat,
        BearerToken,
        "MODELSCOPE_API_KEY",
        Some("https://api-inference.modelscope.cn/v1"),
        "Qwen/Qwen3.5-27B"
    ),
    provider!(
        "doubao",
        "Volcengine Ark (Doubao)",
        OpenAiChat,
        BearerToken,
        "ARK_API_KEY",
        Some("https://ark.cn-beijing.volces.com/api/v3"),
        "doubao-seed-2-0-pro-260215"
    ),
    provider!(
        "mimo",
        "Xiaomi MiMo (Cow compatible)",
        OpenAiChat,
        BearerToken,
        "MIMO_API_KEY",
        Some("https://api.xiaomimimo.com/v1"),
        "mimo-v2.5-pro"
    ),
    provider!(
        "linkai",
        "LinkAI",
        OpenAiChat,
        BearerToken,
        "LINKAI_API_KEY",
        Some("https://api.link-ai.tech/v1"),
        "linkai-4o"
    ),
    provider!(
        "custom-openai",
        "Custom OpenAI-compatible",
        OpenAiChat,
        BearerToken,
        "CUSTOM_API_KEY",
        None,
        "custom-model"
    ),
];

#[must_use]
pub fn provider(id: &str) -> Option<&'static ProviderSpec> {
    BUILTIN_PROVIDERS.iter().find(|provider| provider.id == id)
}

#[must_use]
pub fn provider_options_html() -> String {
    let mut options = String::new();
    for provider in BUILTIN_PROVIDERS {
        options.push_str("<option value=\"");
        options.push_str(provider.id);
        options.push_str("\" data-default-model=\"");
        options.push_str(provider.default_model);
        options.push_str("\">");
        options.push_str(provider.display_name);
        options.push_str("</option>");
    }
    options
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::*;

    #[test]
    fn union_catalog_has_unique_safe_provider_ids_and_every_route_has_a_default_model() {
        let mut ids = BTreeSet::new();
        for provider in BUILTIN_PROVIDERS {
            assert!(ids.insert(provider.id));
            assert!(
                provider
                    .id
                    .bytes()
                    .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
            );
            assert!(!provider.display_name.trim().is_empty());
            assert!(!provider.default_model.trim().is_empty());
            if let Some(base_url) = provider.default_base_url {
                assert!(base_url.starts_with("https://"));
            }
        }
        assert!(ids.len() >= 40);
        for required in [
            "openai",
            "anthropic",
            "openai-codex",
            "github-copilot",
            "amazon-bedrock",
            "google",
            "deepseek",
            "dashscope",
            "qianfan",
            "zhipu",
            "modelscope",
            "doubao",
            "custom-openai",
        ] {
            assert!(ids.contains(required));
        }
    }
}
