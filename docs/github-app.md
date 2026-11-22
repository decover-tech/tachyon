# The Tachyon GitHub App

Customers connect repositories to Tachyon by installing the Tachyon GitHub App. The App asks only for what the GitHub adapter needs ([architecture: Job API and the GitHub Actions adapter](architecture.md#job-api-and-the-github-actions-adapter)). This page covers the permission set, how to register the App, and how a pilot org installs it.

## Permissions and events

The set is defined once, in [`internal/frontend/github/app.go`](../internal/frontend/github/app.go). The App manifest, the scope of every installation token and the client's endpoint allowlist all come from it. Tests fail if the REST client calls an endpoint outside the allowlist, or if the App requests a permission that no endpoint or event uses.

| Repository permission | Access | Why |
| --- | --- | --- |
| Administration | write | Self-hosted runner management. GitHub puts repository-scoped runner endpoints behind this permission and offers nothing narrower. Runners must be repository-scoped, because GitHub hands a queued job to any matching idle runner. The client calls only the three runner endpoints below |
| Actions | read | Look up the workflow run behind a job; receive `workflow_job` events |
| Contents | read | Read repositories for prebuilds, check branch protection for trust, receive `push` events |
| Metadata | read | Granted to every GitHub App by GitHub and cannot be removed |

No organization or account permissions. No secrets, pull request, issue, workflow or deployment access.

| Webhook event | Needs | Used for |
| --- | --- | --- |
| `workflow_job` | Actions | Create a just-in-time runner for queued `tachyon-*` jobs; track progress |
| `push` | Contents | Hint the scheduler to pre-fork job disks for the new commit |

GitHub delivers `installation` and `installation_repositories` events to every App without a subscription. `pull_request` is not subscribed, because it would need Pull requests: read. PR jobs run normally without it. They only lose the pre-fork hint, and pushes to same-repo PR branches still send one through `push`.

Every call the control plane makes:

| Call | Auth | Permission |
| --- | --- | --- |
| `POST /app/installations/{id}/access_tokens` | App JWT | none; the token is requested with exactly the set above |
| `GET /repos/{owner}/{repo}/actions/runs/{run_id}` | installation | Actions: read |
| `GET /repos/{owner}/{repo}/branches/{branch}` | installation | Contents: read |
| `POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig` | installation | Administration: write |
| `GET /repos/{owner}/{repo}/actions/runners` | installation | Administration: read |
| `DELETE /repos/{owner}/{repo}/actions/runners/{runner_id}` | installation | Administration: write |

Each installation token is requested with exactly the App's permissions. The control plane rejects a token whose permissions reach beyond them, so the token stays least-privilege even if someone widens the App's registration by hand in GitHub's settings.

## Registering the App (Tachyon operators)

Register the App once, under the Tachyon GitHub organization, using GitHub's [manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest). The manifest flow sets the permissions and events from code, so they can't be mistyped in the settings UI.

1. Generate the manifest. `-public` lets accounts other than the owner install the App. Pilot orgs need it. Public here does not mean listed on the Marketplace.

   ```sh
   bin/tachyon github-app manifest \
     -name "Tachyon" \
     -homepage https://github.com/decover-tech/tachyon \
     -webhook-url https://<control-plane-host>/webhooks/github \
     -redirect-url https://github.com/decover-tech/tachyon \
     -public > manifest.json
   ```

2. Submit it. The manifest flow takes an HTML form POST, so save this page locally, paste the manifest JSON into the `value`, and open it in a browser while signed in as an owner of the Tachyon org:

   ```html
   <form action="https://github.com/organizations/decover-tech/settings/apps/new?state=<random>" method="post">
     <input type="hidden" name="manifest" value='<contents of manifest.json>'>
     <button type="submit">Create Tachyon GitHub App</button>
   </form>
   ```

   GitHub shows the permissions for review, then redirects to `redirect-url?code=<code>&state=<random>`. Check that `state` matches, then copy `code`. It expires after one hour.

3. Exchange the code for the App's credentials:

   ```sh
   gh api -X POST /app-manifests/<code>/conversions > app.json
   ```

   `app.json` contains the App `id`, `slug`, `pem` (private key) and `webhook_secret`. Put the key and the webhook secret in the key store right away, delete `app.json`, and never commit either.

4. Run the control plane with them (see the README):

   ```sh
   TACHYON_GITHUB_WEBHOOK_SECRET=<webhook_secret> bin/tachyon-controlplane \
     -github-app-id <id> -github-app-key <pem file> \
     -github-allowed-owners pilot-org-a,pilot-org-b
   ```

   `-github-allowed-owners` limits runners to repositories owned by the listed accounts. Anyone can install a public App. Until general availability, only allowlisted pilot orgs get runners. Events from other accounts are acknowledged and ignored.

5. Verify. In the App's settings (`https://github.com/organizations/decover-tech/settings/apps/<slug>/permissions`), the repository permissions must match the table above, with no organization or account permissions, and the subscribed events must be exactly `push` and `workflow_job`. In the Advanced tab, the webhook `ping` should show HTTP 202.

### Changing permissions later

Edit `AppPermissions` and `AppEvents`, extend `Endpoints`, and update this page. Then change the registered App to match in its settings. GitHub asks each installation to approve the new permissions. Until an org approves, token requests for that installation fail, because the control plane asks for the full set, and that installation's jobs stay queued. Tell pilot orgs before shipping a permission change.

## Installing it (pilot orgs)

What to send a pilot org's admin:

1. **Install.** An organization owner opens `https://github.com/apps/<slug>/installations/new`, picks the organization, and chooses **Only select repositories**. Start with one or two private repositories. GitHub lists the permissions above before the install is confirmed.
2. **Tell us the org login.** Tachyon adds it to `-github-allowed-owners`. Until then, jobs labeled `tachyon-*` stay queued on GitHub.
3. **Opt jobs in.** In each workflow, change `runs-on` to a Tachyon label such as `tachyon-ubuntu-24.04-8x`. Optionally add `.tachyon/prebuild.yml` and the `tachyon/warm` step ([README](../README.md#using-it-in-a-repo), [example](../examples/node-go)). Jobs without a `tachyon-` label are never touched.
4. **Check it works.** Push a commit. The job should be picked up by a runner named `tachyon-…`. These runners appear briefly under the repository's *Settings → Actions → Runners* and are removed after each job.

Notes for the pilot org:

- Runners are repository-scoped, single-use and just-in-time. Each job gets a fresh VM, which is destroyed afterward.
- Pull requests from forks run as `untrusted-fork`: they can read prebuilds and caches, but never write prebuilds, sticky volumes or shared cache entries. Self-hosted runners on public repositories still deserve care, so we recommend private repositories for the pilot.
- **Adding repositories:** *Organization settings → GitHub Apps → Tachyon → Configure → Repository access*.
- **Removing Tachyon:** uninstall the App from the same page. Tachyon loses API access at once, so any runner registered at that moment can't be cleaned up by Tachyon. Remove leftovers under the repository's *Settings → Actions → Runners*.
