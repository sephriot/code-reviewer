# Code reviewer

## Tech stack:
- Go
- Go templates for frontend Web UI
- Database: SQLite (schema is for you to define, keep it simple)

## Authentication & GitHub
User delivers GitHub token via environment variable.
Use whatever GitHub API seems viable. If we get rate limited it is enough to display toast notification.



## Functional requirements
This is a standalone app, meant to run on a single computer with exactly 1 user.
User interacts with the app via Web UI.
App has 2 loops: 
1. Scanning loop - periodically executed (e.g. every 1 minute). Go over GitHub and reads open PRs assigned to a given user, stores them locally marking whether PR is still open and whether it still needs review (if the user left a review in GitHub already, it should not be marked as "needs review"). Once scan and reconciliation is done, it stores review_requests in a separate table. Once done, and there are new review_requests, signal reaction loop to start. If a PR gets a new commit it is meant to be reviewed again. Mark the previous record as outdated and record a new one. Each PR should be uniquely identified by commit SHA and repository combination.
2. Reaction loop - periodically executed (e.g. every 1 minute or on demand). Go over review requests table and executed them one by one, by invoking tools like `agent`, `codex` or `claude`. All of these tools are called via shell command execution. Timeout for a single review is 15 minutes. Once that tool finishes work, it parses the output (either success or error) and assigns the result to the PR. The review can have 4 outcomes, 3 successful ones are - approve_without_comments, approve_with_comments, human_review. The failure scenario - tool execution failed - is the fourth outcome and should be surfaced in the UI. PR reviews are sequential, only one run at a time, so review_requests table is kind of a persistent queue. User is meant to supply (define a path in the config) to review prompt, a prompt used to instruct the tool how to conduct review. Some prompt examples are in @prompts directory.
There should be two notification flavours (configurable) emitted, a TTS notification via `say` tool from macOS (app will be used on mac exclusively) and/or a browser notification. 
Say notifications should be templatable, user should be able to define what exactly is being played upon notification.
Notifications should be emitted upon following events:
- PR review start
- PR review finish (success depending on outcome and fail have distinct notifications) e.g., PR needs human attention, PR review failed, PR {title} by {author} approved without comments.
Use placeholder syntax (e.g. {author} {title})
User should grant privilege for browser notification on first visit.

Notification templates are part of application config.
Scanned pull requests should be possible to be filtered on the config file level by repository (or organization) and author. Both cases (repository and author filter), should apply regular expressions or comma separated values. (e.g. wildcard for entire organization, authors enumerated).
PRs filtering should be visible in logs.
Draft PRs are skipped by default, but leave an option to review them as well.
PRs filtered out should be visible somewhere in the UI (not on the main page) and user should still be able to click a "request review" for them. So in a way, user has a manual way of requesting a review for a single PR. 

The review for given PR is visible in the UI, user can click a button to publish it to GitHub, request another round of review (re-invokes the review tool on the same PR), edit and delete comments.
Publishing: user can choose between publishing inline comments only, or the full review (review outcome + inline comments + general review comment) as a GitHub PR Review via API.

The app is meant to store historical data, so if a PR is reviewed or closed we still keep it in the DB, just mark as finished. There is no pruning planned, operator might delete old entities on his own if he wishes, otherwise we keep everything. Make sure to keep meaningful fields like created_at, updated_at, deleted_at for each entity to ease this process.

The app should also have a capability of reviewing my own PRs in addition to those I am requested to review. It should not be turned on by default, I should be able to enable it by changing the config.

## Startup behavior
Both loops start.

## Error handling
If the error was a result of an UI action show a toast.
If the error was a result of backend loop - just log it.

## Analytics
User can see analytics (charts) about how many PRs were reviewed over time, divided by review result and PR author and repository. Basically mix and match defined by user with reasonable default view.
Dimensions:
- time
- repository
- author
- Review status (fail / success with no comments / success with comments / human review required).

Default chart period - last 30 days.
Other options:
- last week
- last month
- last quarter
- last year
- all time

## Telemetry
App should log every event that happens in it, so the operator has some insight into what is being executed in given moment in time.
Logs are simple unstructured logs sent to STD out

## Web UI
- paths, routes and design are for you to come up with. The design should be minimalistic, avoid unnecessary elements or complexity. Having that said, I don't want it to be raw HTML, style it, just make sure the overall look is lightweight.
- Communication can either use HTTP requests with periodic updates of selected fields (do not refresh page!) or websockets, it's up to you to decide.

## Config
I'll be using environment variables to configure the app, there can be a YAML based alternative.
Precedence:
- Env variables
- yaml config if supplied (user defines path in CLI flag, format is for you to define, keep it simple)
- inline server flags of the server app

## Tool output contract
Take a look at @prompts/output_format.md and distil the prompt from it. Keep the prompt as integral part of the app (embed it in the code), this is meant to force review tools to return valid output the app can read.

## Non goals
multi user, webhooks, ci, slack / email. Keep the app simple, the simpler it is the better.

## General guidelines
Follow go idiomatic programming, keep clear boundaries between packages. Keep it simple.