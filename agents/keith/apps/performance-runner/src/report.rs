use std::collections::BTreeMap;

use serde::Serialize;

#[derive(Clone, Debug, Serialize)]
pub struct LatencySummary {
    pub samples: usize,
    pub minimum_micros: u64,
    pub p50_micros: u64,
    pub p95_micros: u64,
    pub p99_micros: u64,
    pub maximum_micros: u64,
}

#[derive(Clone, Debug, Default)]
pub struct Measurements {
    values: BTreeMap<String, Vec<u64>>,
}

impl Measurements {
    pub fn record(&mut self, name: impl Into<String>, micros: u64) {
        self.values.entry(name.into()).or_default().push(micros);
    }

    pub fn extend(&mut self, other: Self) {
        for (name, values) in other.values {
            self.values.entry(name).or_default().extend(values);
        }
    }

    pub fn summaries(&self) -> BTreeMap<String, LatencySummary> {
        self.values
            .iter()
            .map(|(name, values)| (name.clone(), summarize(values)))
            .collect()
    }

    pub fn raw(&self) -> BTreeMap<String, Vec<u64>> {
        self.values.clone()
    }
}

pub fn summarize(values: &[u64]) -> LatencySummary {
    let mut ordered = values.to_vec();
    ordered.sort_unstable();
    LatencySummary {
        samples: ordered.len(),
        minimum_micros: ordered.first().copied().unwrap_or(0),
        p50_micros: percentile(&ordered, 50),
        p95_micros: percentile(&ordered, 95),
        p99_micros: percentile(&ordered, 99),
        maximum_micros: ordered.last().copied().unwrap_or(0),
    }
}

fn percentile(ordered: &[u64], percentile: usize) -> u64 {
    if ordered.is_empty() {
        return 0;
    }
    let numerator = percentile
        .saturating_mul(ordered.len().saturating_sub(1))
        .saturating_add(99);
    ordered[numerator / 100]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn summaries_are_deterministic_and_keep_tail_latency_visible() {
        let values = (1..=100).collect::<Vec<_>>();
        let summary = summarize(&values);
        assert_eq!(summary.samples, 100);
        assert_eq!(summary.minimum_micros, 1);
        assert_eq!(summary.p50_micros, 51);
        assert_eq!(summary.p95_micros, 96);
        assert_eq!(summary.p99_micros, 100);
        assert_eq!(summary.maximum_micros, 100);
    }
}
