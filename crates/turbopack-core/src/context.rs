use turbo_tasks::{Value, Vc};
use turbo_tasks_fs::FileSystemPath;

use crate::{
    compile_time_info::CompileTimeInfo,
    module::OptionModule,
    reference_type::ReferenceType,
    resolve::{options::ResolveOptions, parse::Request, ModuleResolveResult, ResolveResult},
    source::Source,
};

/// A context for building an asset graph. It's passed through the assets while
/// creating them. It's needed to resolve assets and upgrade assets to a higher
/// type (e. g. from FileSource to ModuleAsset).
#[turbo_tasks::value_trait]
pub trait AssetContext {
    /// Gets the compile time info of the asset context.
    fn compile_time_info(self: Vc<Self>) -> Vc<CompileTimeInfo>;

    /// Gets the layer of the asset context.
    fn layer(self: Vc<Self>) -> Vc<String>;

    /// Gets the resolve options for a given path.
    fn resolve_options(
        self: Vc<Self>,
        origin_path: Vc<FileSystemPath>,
        reference_type: Value<ReferenceType>,
    ) -> Vc<ResolveOptions>;

    /// Resolves an request to an [ModuleResolveResult].
    fn resolve_asset(
        self: Vc<Self>,
        origin_path: Vc<FileSystemPath>,
        request: Vc<Request>,
        resolve_options: Vc<ResolveOptions>,
        reference_type: Value<ReferenceType>,
    ) -> Vc<ModuleResolveResult>;

    /// Process a source into a module. This might return None, if this should
    /// lead to no module at all, e. g. in cases where side effect free module
    /// is imported for its side effects.
    fn process(
        self: Vc<Self>,
        asset: Vc<Box<dyn Source>>,
        reference_type: Value<ReferenceType>,
    ) -> Vc<OptionModule>;

    /// Process an [ResolveResult] into an [ModuleResolveResult].
    fn process_resolve_result(
        self: Vc<Self>,
        result: Vc<ResolveResult>,
        reference_type: Value<ReferenceType>,
    ) -> Vc<ModuleResolveResult>;

    /// Gets a new AssetContext with the transition applied.
    fn with_transition(self: Vc<Self>, transition: String) -> Vc<Box<dyn AssetContext>>;
}
