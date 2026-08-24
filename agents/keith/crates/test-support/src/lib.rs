#![forbid(unsafe_code)]

use serde::{Deserialize, Serialize};
use std::collections::VecDeque;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PrivateFixtureContent;

impl std::fmt::Display for PrivateFixtureContent {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("fixture contains credential-shaped or private content")
    }
}

/// Rejects credential-shaped fields and private-content markers after JSON decoding.
/// JSON encoded inside a string is decoded and inspected recursively as well.
///
/// # Errors
///
/// Returns [`PrivateFixtureContent`] when a key or value is credential-shaped or private.
pub fn ensure_public_fixture(value: &serde_json::Value) -> Result<(), PrivateFixtureContent> {
    fn contains_private_content(value: &serde_json::Value) -> bool {
        match value {
            serde_json::Value::Object(fields) => fields.iter().any(|(key, value)| {
                let normalized = key
                    .chars()
                    .filter(char::is_ascii_alphanumeric)
                    .flat_map(char::to_lowercase)
                    .collect::<String>();
                [
                    "authorization",
                    "apikey",
                    "password",
                    "secret",
                    "privatekey",
                ]
                .iter()
                .any(|marker| normalized.contains(marker))
                    || contains_private_content(value)
            }),
            serde_json::Value::Array(values) => values.iter().any(contains_private_content),
            serde_json::Value::String(text) => {
                let normalized = text.to_ascii_lowercase();
                [
                    "authorization:",
                    "bearer ",
                    "api_key",
                    "api-key",
                    "password=",
                    "secret=",
                    "personal memory",
                    "private user",
                    "private reasoning",
                    "-----begin private key-----",
                ]
                .iter()
                .any(|marker| normalized.contains(marker))
                    || serde_json::from_str::<serde_json::Value>(text)
                        .is_ok_and(|decoded| contains_private_content(&decoded))
            }
            _ => false,
        }
    }

    if contains_private_content(value) {
        Err(PrivateFixtureContent)
    } else {
        Ok(())
    }
}

/// A reusable one-shot source for deterministic regression tests.
#[derive(Clone, Debug)]
pub struct RecordedTape<T> {
    remaining: VecDeque<T>,
    observed: Vec<T>,
}

impl<T> RecordedTape<T> {
    #[must_use]
    pub fn new(values: impl IntoIterator<Item = T>) -> Self {
        Self {
            remaining: values.into_iter().collect(),
            observed: Vec::new(),
        }
    }

    #[must_use]
    pub fn is_exhausted(&self) -> bool {
        self.remaining.is_empty()
    }

    #[must_use]
    pub fn remaining(&self) -> usize {
        self.remaining.len()
    }

    #[must_use]
    pub fn observed(&self) -> &[T] {
        &self.observed
    }
}

impl<T: Clone> RecordedTape<T> {
    #[must_use]
    pub fn take_next(&mut self) -> Option<T> {
        let value = self.remaining.pop_front()?;
        self.observed.push(value.clone());
        Some(value)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RegressionComparison {
    Improved,
    Equivalent,
    Regressed,
}

/// Result of executing the same deterministic regression case twice.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RegressionRun<T, E> {
    Deterministic(Result<T, E>),
    Inconclusive {
        first: Result<T, E>,
        second: Result<T, E>,
    },
}

/// Reusable two-run harness for ordinary deterministic regression tests.
#[derive(Clone, Copy, Debug, Default)]
pub struct RegressionHarness;

impl RegressionHarness {
    pub fn run<T: Eq, E: Eq>(mut candidate: impl FnMut() -> Result<T, E>) -> RegressionRun<T, E> {
        let first = candidate();
        let second = candidate();
        if first == second {
            RegressionRun::Deterministic(first)
        } else {
            RegressionRun::Inconclusive { first, second }
        }
    }
}

#[must_use]
pub fn compare_regression(
    baseline_cost: u64,
    baseline_latency_ms: u64,
    cost: u64,
    latency_ms: u64,
) -> RegressionComparison {
    if cost <= baseline_cost && latency_ms <= baseline_latency_ms {
        if cost < baseline_cost || latency_ms < baseline_latency_ms {
            RegressionComparison::Improved
        } else {
            RegressionComparison::Equivalent
        }
    } else {
        RegressionComparison::Regressed
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recorded_tape_is_one_shot_and_records_exact_observations() {
        let mut tape = RecordedTape::new([1_u8, 2, 3]);
        assert_eq!(tape.remaining(), 3);
        assert_eq!(tape.take_next(), Some(1));
        assert_eq!(tape.take_next(), Some(2));
        assert_eq!(tape.observed(), &[1, 2]);
        assert!(!tape.is_exhausted());
        assert_eq!(tape.take_next(), Some(3));
        assert_eq!(tape.take_next(), None);
        assert!(tape.is_exhausted());
    }

    #[test]
    fn public_fixture_check_inspects_decoded_keys_and_nested_json_strings() {
        assert!(ensure_public_fixture(&serde_json::json!({"result": "safe"})).is_ok());
        assert!(ensure_public_fixture(&serde_json::json!({"api_key": "value"})).is_err());
        assert!(
            ensure_public_fixture(&serde_json::json!({
                "result": "{\"api\\u005fkey\":\"value\"}"
            }))
            .is_err()
        );
    }

    #[test]
    fn regression_harness_preserves_complete_results_and_detects_divergence() {
        assert_eq!(
            RegressionHarness::run(|| Ok::<_, String>("stable")),
            RegressionRun::Deterministic(Ok("stable"))
        );
        let mut run = 0;
        assert_eq!(
            RegressionHarness::run(|| {
                run += 1;
                Err::<(), _>(format!("failure-{run}"))
            }),
            RegressionRun::Inconclusive {
                first: Err("failure-1".into()),
                second: Err("failure-2".into()),
            }
        );
    }
}
