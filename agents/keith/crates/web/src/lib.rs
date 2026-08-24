#![forbid(unsafe_code)]

mod browser;
mod fetch;

pub use browser::{
    AuthorizedBrowserAction, BrowserControlBounds, BrowserDownloadRequest, BrowserError,
    BrowserPolicy, BrowserProgress, BrowserProgressSink, BrowserProgressSinks, BrowserRunner,
    BrowserSessionSummary, ConfirmationProvider, ConfirmationRequest, ConsequentialAction,
    HeadedBrowserLaunch, NoBrowserProgress, SemanticLink, SemanticObservation,
};
pub use fetch::{
    DestinationResolver, DestinationValidator, FetchProgress, FetchProgressSink, FetchResponse,
    NoFetchProgress, SafeWebClient, SystemDestinationResolver, ValidatedDestination, WebError,
    WebPolicy, is_public_ip,
};
