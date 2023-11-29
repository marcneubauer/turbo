//! Turborepo's library for high quality errors

use std::{fmt::Display, sync::Arc};

use serde::{Deserialize, Serialize};

#[derive(Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum Provenance {
    // TODO: Add line/column numbers
    TurboJson,
    EnvironmentVariable { name: String },
    Flag { name: String },
}

impl Display for Provenance {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Provenance::TurboJson => write!(f, "from turbo.json"),
            Provenance::EnvironmentVariable { name } => write!(f, "environment variable {}", name),
            Provenance::Flag { name } => write!(f, "flag --{}", name),
        }
    }
}

impl Provenance {
    pub fn from_flag(name: impl Into<String>) -> Option<Arc<Provenance>> {
        Some(Arc::new(Provenance::Flag { name: name.into() }))
    }
}
