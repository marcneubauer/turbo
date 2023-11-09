pub enum Provenance {
    // TODO: Add line/column numbers
    TurboJson,
    EnvironmentVariable { name: String },
    Flag { name: String },
}

pub trait ErrorProvenance {
    fn provenance(&self) -> Option<Provenance>;
}
