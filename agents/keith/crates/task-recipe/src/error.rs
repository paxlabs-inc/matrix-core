use thiserror::Error;

#[derive(Debug, Error)]
pub enum TaskRecipeError {
    #[error("demonstration is invalid: {0}")]
    InvalidDemonstration(String),
    #[error("task recipe is invalid: {0}")]
    InvalidRecipe(String),
    #[error("capture or persistence limit exceeded: {0}")]
    LimitExceeded(String),
    #[error("operation is invalid in the current lifecycle state")]
    InvalidState,
    #[error("requested demonstration, recipe, or media does not exist")]
    NotFound,
    #[error("recipe publication requires passed checks and explicit user acceptance")]
    PublicationNotReady,
    #[error("task recipe I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("task recipe JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("task recipe TOML failed: {0}")]
    Toml(#[from] toml::ser::Error),
    #[error("skill publication failed: {0}")]
    Skill(#[from] keith_skills::SkillError),
}
