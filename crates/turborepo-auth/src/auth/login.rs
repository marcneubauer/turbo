use std::{borrow::Cow, sync::Arc};

pub use error::Error;
use reqwest::Url;
use tokio::sync::OnceCell;
use tracing::warn;
use turborepo_api_client::Client;
use turborepo_ui::{start_spinner, BOLD, UI};

use crate::{error, get_token_for, server::LoginServer, ui};

const DEFAULT_HOST_NAME: &str = "127.0.0.1";
const DEFAULT_PORT: u16 = 9789;

/// Login writes a token to disk at token_path. If a token is already present,
/// we do not overwrite it and instead log that we found an existing token.
pub async fn login<'a>(
    api_client: &impl Client,
    ui: &UI,
    existing_token: Option<&'a str>,
    login_url_configuration: &str,
    login_server: &impl LoginServer,
) -> Result<Cow<'a, str>, Error> {
    // Check if token exists first.
    if let Some(token) = existing_token {
        if let Ok(response) = api_client.get_user(token).await {
            println!("{}", ui.apply(BOLD.apply_to("Existing token found!")));
            ui::print_cli_authorized(&response.user.email, ui);
            return Ok(token.into());
        }
    }

    let redirect_url = format!("http://{DEFAULT_HOST_NAME}:{DEFAULT_PORT}");
    let mut login_url = Url::parse(login_url_configuration)?;

    login_url
        .path_segments_mut()
        .map_err(|_: ()| Error::LoginUrlCannotBeABase {
            value: login_url_configuration.to_string(),
        })?
        .extend(["turborepo", "token"]);

    login_url
        .query_pairs_mut()
        .append_pair("redirect_uri", &redirect_url);

    println!(">>> Opening browser to {login_url}");
    let spinner = start_spinner("Waiting for your authorization...");

    // Try to open browser for auth confirmation.
    let url = login_url.as_str();
    if !cfg!(test) && webbrowser::open(url).is_err() {
        warn!("Failed to open browser. Please visit {url} in your browser.");
    }

    let token_cell = Arc::new(OnceCell::new());
    login_server
        .run(
            DEFAULT_PORT,
            login_url_configuration.to_string(),
            token_cell.clone(),
        )
        .await?;

    spinner.finish_and_clear();

    let token = token_cell.get().ok_or(Error::FailedToGetToken)?;

    // TODO: make this a request to /teams endpoint instead?
    let user_response = api_client
        .get_user(token.as_str())
        .await
        .map_err(Error::FailedToFetchUser)?;

    ui::print_cli_authorized(&user_response.user.email, ui);

    Ok(token.to_string().into())
}

#[cfg(test)]
mod tests {
    use turborepo_vercel_api_mock::start_test_server;

    use super::*;
    use crate::mocks::*;

    #[tokio::test]
    async fn test_login() {
        let port = port_scanner::request_open_port().unwrap();
        let api_server = tokio::spawn(start_test_server(port));
        let ui = UI::new(false);
        let url = format!("http://localhost:{port}");

        let api_client = MockApiClient::new();

        let login_server = MockLoginServer {
            hits: Arc::new(0.into()),
        };

        let token = login(&api_client, &ui, None, &url, &login_server)
            .await
            .unwrap();

        let got_token = Some(token.to_string());

        // Token should be set now
        assert_eq!(
            got_token.as_deref(),
            Some(turborepo_vercel_api_mock::EXPECTED_TOKEN)
        );

        // Call the login function a second time to test that we check for existing
        // tokens. Total server hits should be 1.
        let second_token = login(&api_client, &ui, got_token.as_deref(), &url, &login_server)
            .await
            .unwrap();

        // We can confirm that we didn't fetch a new token because we're borrowing the
        // existing token and not getting a new allocation.
        assert!(second_token.is_borrowed());

        api_server.abort();
        assert_eq!(
            login_server.hits.load(std::sync::atomic::Ordering::SeqCst),
            1
        );
    }
}
