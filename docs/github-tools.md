# GitHub tools for the agents

Status: proposal, nothing implemented. It proposes that redoma give agents
GitHub access through a small set of tools served on the host, instead of a
`gh` CLI, a `git push` and a credential inside the container.

## Why

An agent that works on a repository needs to publish its work: push a
branch, open a pull request, answer review comments, read the checks. Today
the only way to do that from a redoma container is to give it what a person
would use: a token in its environment or a credential helper, `gh`, and the
GitHub hosts in its egress allowlist (`egress/<name>.txt`). That undoes
what redoma does for the model credential:

- **The container holds the credential.** Anything the turn runs can read
  it, copy it into a file, a commit or a comment, and use it for anything
  its scopes allow, for as long as it is valid. The model key, by contrast,
  never enters the container: Envoy injects it on two paths only.
- **Policy is GitHub's alone.** A token's scopes are coarse: a token that
  can push a branch can push any branch it is not stopped from by a branch
  rule, open pull requests anywhere it has write, and, with the wrong
  scopes, merge, approve, change workflows or delete refs. Nothing on the
  host can say "this agent, this repository, branches under this prefix,
  never `.github/workflows`".
- **Nothing on the host sees what was done.** The egress proxy sees a
  CONNECT to `api.github.com`, not which pull request was opened.

The proposal: the container gets no GitHub credential and no route to
GitHub. A host service, which knows the agent from its SPIFFE ID the way
the relay does, serves a few narrow tools, checks each call against the
agent's policy in code, makes the call with a credential the container
never sees, and logs it.

## Goals and non-goals

Goals:

- No GitHub credential in any agent container, and, once the tools cover
  an agent's work, no GitHub host in its egress allowlist.
- A tool set made for an agent's job and nothing more: publish a branch,
  open and follow its pull requests, talk in them.
- Policy per agent, from host configuration, enforced in code; an error
  never widens access.
- Every call audited with the agent's identity.
- Works in both identity modes (`identity_api = delegated` and `broker`).

Non-goals:

- Minting GitHub credentials. redoma reads tokens that something else
  keeps fresh; this document defines the files, not the minting.
- Merging, reviewing, approving, or any repository administration. Those
  stay with people and with whatever review process the host's owner runs.
- A general GitHub API gateway, or other forges (the design leaves room for
  them, see "Open questions").

## Where the tools are served

The agent's side is the same either way: claude calls MCP tools of
`redoma-shim mcp`, the server it already runs for `ask_lead` and
`send_message`, and the shim forwards each call to the host. The question
is which host process serves them.

### Option A: the relay

`redoma relay` already authenticates each agent by its SVID, over the
agent's relay listener and cluster. A GitHub call would be a new envelope
on the agent's stream, or a new unary service next to `Relay`.

- For: no new service, user, SPIRE entry or cluster.
- Against:
  - **The relay's sandbox is AF_UNIX only** (`RestrictAddressFamilies=AF_UNIX`,
    `IPAddressDeny=any`, no capabilities). GitHub is on the internet; the
    relay would have to open AF_INET and egress, for every one of its
    duties.
  - **The relay would hold GitHub credentials and read every agent's
    workspace** (to push, see "Pushing from the host"). It is the lead's
    channel: a bug in the GitHub code would sit in the process that holds
    every agent's queue and the lead's socket, and the reverse.
  - **The stream is the wrong shape.** It is at-least-once and journaled,
    built for tasks and messages that must not be lost. A tool call is a
    request and a response that must not be done twice (a comment posted
    twice, two pull requests); replaying it after a reconnect is a bug, and
    journaling its result is useless.

### Option B: a sibling service, `redoma github`

A new subcommand and unit, `redoma-github`, under its own user (proposed
`redoma-github`, uid 965), with its own SVID, `spiffe://<td>/host/github`,
from the Workload API (SPIRE attests it by uid, as it does the relay).
It serves a small gRPC service, `redoma.github.v1.GitHub`, on
`/run/redoma/github/github.sock`, reached by the agent through Envoy over
mutual TLS with the agent's SVID, exactly like the relay:

- **Envoy:** the agent's relay listener (127.0.0.1:7070) gets a second
  route, `/redoma.github.v1.GitHub/` to a per-agent cluster
  `agent_<name>_github` (the agent's SVID as client certificate, requiring
  `spiffe://<td>/host/github` of the server), with a route timeout. One
  listener, no new port in the container; the route and cluster exist only
  for agents whose policy enables the tools.
- **The client certificate is the identity, and nothing else is**, as for
  the relay: the bundle, exactly one URI SAN `spiffe://<td>/agent/<name>`,
  a name `agents.txt` allows (`relay/internal/mtls`, shared).
- **Unary, at most once.** Each call carries an id the shim makes; the
  service keeps the ids of recent writes per agent and answers a repeated id
  with the first answer, so a shim that retries after a timeout does not
  post twice.

- For: least privilege. The GitHub credentials, the egress to GitHub and
  read access to the workspaces belong to one uid that holds nothing else;
  the relay keeps its AF_UNIX-only sandbox. Its failures and restarts are
  its own. It can be left uninstalled.
- Against: one more unit, user, SPIRE entry and cluster; the hook's
  `ServiceIDs` and the package's `postinst` id check gain an id.

### Recommendation

Option B. The relay's sandbox and its role as the lead's channel are worth
more than one fewer unit, and the stream's delivery guarantee is the
opposite of what a tool call needs.

### Why the shim, and not claude straight to the host

Claude Code can also talk to an MCP server over HTTP, so the host service
could itself be an MCP server behind the Envoy route, with no shim in
between. The proposal keeps the shim as the in-container face:

- One MCP server for claude, whose tool list and `--allowedTools` the shim
  already controls (the github tools are added only when the shim runs
  with `--github`).
- The host parses protobuf of a fixed, documented schema (the proto rules
  in AGENTS.md apply), not JSON-RPC and MCP's evolving protocol, on the
  process that holds the credentials.
- The shim can add context the host cannot know, such as the task id the
  call belongs to (for the audit log; claimed by the shim, so marked
  unverified) and refuse a call outside a turn.

## The tools

All tools take `repo` as `owner/name`, which must be in the agent's policy.
Names below are MCP names; each has one rpc.

| Tool | What it does | Credential |
|---|---|---|
| `github_push(repo, branch)` | pushes the workspace's local branch to the same name on GitHub, fast-forward only (see "Pushing from the host") | `push` |
| `github_open_pr(repo, branch, base?, title, body, draft?)` | opens a pull request from `branch`; if one is open for it, returns that one | `pr` |
| `github_update_pr(repo, number, title?, body?)` | edits the title or body of a pull request the agent may write to | `default` |
| `github_comment(repo, number, body)` | comments on a pull request or issue the agent may write to | `default` |
| `github_reply(repo, number, comment_id, body)` | replies in a review thread | `default` |
| `github_read_pr(repo, number, parts, max_bytes?)` | metadata, the diff, the checks and statuses, the review threads (any of them) | `default` |
| `github_list_prs(repo, branch?, state?)` | the pull requests of a head branch, or the agent's own | `default` |

"The agent's own" pull requests, and those it "may write to", are by
default the ones whose head branch has the agent's branch prefix in the same
repository; a policy can widen comments to any pull request or issue of the
repository (`write_scope`).

Not offered, on purpose, and not reachable through any argument:

- merging, enabling auto-merge, updating a branch from its base;
- reviews of any kind: approve, request changes, submit or dismiss;
- commit statuses and check runs (a status is how a repository's owner
  says "this was reviewed" or "this passed"; an agent must not say it about
  its own work);
- anything under Actions: running, re-running or cancelling workflows,
  reading or writing secrets and variables;
- repository administration: settings, collaborators, branch rules,
  webhooks, deploy keys, releases and tags;
- deleting refs or branches, force-pushing, pushing tags;
- resolving review threads (whether a finding is addressed is the
  reviewer's call);
- creating or editing files through the contents API (it would bypass the
  push checks below).

Each is one more rpc if a host ever needs it, and the absence of an rpc is
a stronger guarantee than a policy switch.

### Why not GitHub's own MCP server

GitHub publishes an MCP server, `github-mcp-server`, that could be run on
the host with the credential and reached by claude through Envoy. It is not
proposed, for these reasons (as of this writing):

- **Its toolsets are coarse.** Tools are enabled per toolset
  (`pull_requests`, `repos`, `issues`, `actions`, ...). The `pull_requests`
  toolset holds merging and submitting reviews next to opening a pull
  request; `repos` holds creating and updating files and branches through
  the API next to reading them. Its read-only switch is all or nothing.
  The tool set redoma wants is a few tools across several toolsets, minus
  the dangerous ones of each.
- **It enforces no policy.** It does what the token allows, in any
  repository and on any branch the token reaches; there is no notion of an
  agent, a branch prefix or a protected path.
- **One credential.** It cannot open pull requests with one token and do
  everything else with another.
- **It cannot push a workspace.** Its write path is the contents API
  (files sent as tool arguments, one commit per call, authored by the
  token), not the agent's commits.
- **It changes under you.** New tools and arguments arrive with its
  releases, and a dynamic mode can enable toolsets at run time.

A filtering proxy in front of it (parse MCP JSON-RPC, allow tools by name,
check each tool's arguments against the policy) would cost about as much
as the tools themselves: one argument checker per allowed tool, kept in
step with upstream's schemas, a refusal for any tool or argument it does
not know, the credential still in a process redoma does not control, and
still no host-side push or second credential. redoma's own tools are a few
hundred lines over the REST API, tested against a fake, and change only
when redoma changes them.

## Policy

Per agent, from the host's configuration, read by `redoma github` only and
re-read on change, like the egress allowlists:
`/etc/redoma/github/_default.conf` (every agent's), then
`/etc/redoma/github/<name>.conf`. An agent with no file of its own has no
tools. Same format as `redoma.conf` (`key = value`, `#` comments; a list
is comma-separated), with the same strictness: an unknown key, a key given
twice or a value that does not parse makes the agent's whole policy
invalid, and an invalid policy refuses every call ("An error never widens
access"). The directory and files are root's and writable by no one else,
like `agents.txt`.

```
# /etc/redoma/github/example.conf
repos = example-org/example-repo
# The agent's workspace on the host, and the directory of each repository's
# clone in it (see "Pushing from the host").
workspace = /srv/agents/example/work
checkout.example-org/example-repo = example-repo
branch_prefix = agent/example/
protected_branches = release/
# Bases a pull request may target; empty means the default branch only.
pr_bases =
protected_paths = .github/workflows/, .github/actions/
commit_author = Example Agent <agent@example.org>
require_signoff = true
max_commits = 100
max_push_bytes = 52428800
max_blob_bytes = 10485760
max_title_bytes = 256
max_body_bytes = 65536
write_scope = own
credential.push = app
credential.pr = pr
credential.default = app
rate.writes_per_minute = 6
rate.prs_per_day = 20
```

What each enforces:

- **`repos`**: the only repositories any tool takes. Matched exactly,
  case-insensitively as GitHub does, after the service has resolved the
  name (a renamed repository's redirect is not followed into a repository
  that is not listed).
- **`branch_prefix`**: every branch the agent pushes or opens a pull request
  from starts with it. The service refuses, at load, two agents whose
  prefixes on one repository overlap (one a prefix of the other), so an
  agent never writes another's branches. The default branch and any branch
  under `protected_branches` are refused even if the prefix would match.
- **`pr_bases`**: the branches a pull request may target.
- **`protected_paths`**: no pushed commit may add, change, delete or rename
  to or from a path under any of them (see "What a push checks").
  `.github/workflows/` is in the built-in default; a host can add to it, and
  removing it needs an explicit `protected_paths =` in the agent's file.
- **`commit_author`, `require_signoff`**: the identity every pushed commit
  must carry.
- **Size limits**: commits per push, pack bytes, the largest blob, the
  title and body of a pull request or comment.
- **`write_scope`**: `own` (pull requests whose head has the prefix) or
  `repo` (any pull request or issue of the listed repositories).
- **`credential.*`**: which credential each class of call uses (see
  "Credentials").
- **Rate limits**: per agent, a token bucket for writes and a daily cap on
  pull requests; the service also honours GitHub's own rate-limit headers
  and secondary limits, globally, by waiting or refusing with the reset time
  rather than retrying blindly.

### Text from GitHub is untrusted

`github_read_pr` returns what other people wrote: review comments, issue
text, check output. On a public repository anyone can comment, so a
comment is a prompt-injection channel into the agent. The tools label every
piece of returned text with its author and the author's association with
the repository (`OWNER`, `MEMBER`, `CONTRIBUTOR`, `NONE`, ...), and a
policy can drop text from authors outside a list (`trusted_authors`) or
below an association. This does not make the text safe; it lets the agent
and its instructions tell the maintainer's review from a stranger's.

## Credentials

Credentials never enter the container, and the container never reaches
GitHub, so a credential is only ever used by `redoma github`, for a call
the policy allowed.

**Two roles by default**, because a host may want them apart:

- `app`: a GitHub App installation token (contents and pull requests write,
  issues write for comments, checks and statuses read, metadata read; never
  `workflows`, administration or secrets). Pushes, comments, replies, reads.
  Short-lived (an hour), scoped to the installation's repositories.
- `pr`: a person's fine-grained token with pull requests write on the
  listed repositories only, used for opening pull requests and nothing
  else. A pull request opened by a person is attributed to them, and so is
  its squash merge: the published history names a person who answers for
  it, with the agent's commits as its contents.

A host that wants one credential for everything maps all three
`credential.*` keys to it. A credential that is named but missing, unreadable
or expired refuses the calls that need it; the service never falls back to
another credential.

**Files.** Each credential is a file, named in `/etc/redoma/github/_credentials.conf`:

```
app = /run/redoma/github/credentials/app.json
pr = /run/redoma/github/credentials/pr.json
```

```json
{"token": "<the token>", "expires_at": "2026-01-01T12:00:00Z"}
```

- `expires_at` is RFC 3339 and required (a token with no expiry is refused:
  a fine-grained token has one, and an App token's is an hour away).
- The file is replaced whole (written aside and renamed), a regular file,
  not a symlink, owned by root or the minter's user, group `redoma-github`,
  mode 0440 at most; the directory root's, 0750, group `redoma-github`.
  Under `/run/redoma`, so the OCI hook refuses to mount it into any
  container.
- The service reads the file at each call that needs it (cheap; or keeps it
  while its inode and mtime are unchanged), so a renewed token is used at
  once and no restart is needed.
- A token that expires in less than `credential_min_validity` (two minutes
  by default) is not used: the call fails with "credential `app` expires at
  ...; renew it", never with the token.
- A GitHub answer of 401 marks the credential bad until its file changes,
  and is logged once.

**The minter** is outside redoma: whatever signs the App's JWT and exchanges
it for an installation token, or rotates the person's token, writes these
files ahead of expiry (an App token at half its life). redoma documents the
files and their checks, ships no minter, and logs a warning when a
credential is within ten minutes of expiry and has not been renewed.

**Never logged, never in an argument list.** Tokens are not logged, not
put in an error, and never passed on a command line (anyone on the host
can read another process's `/proc/<pid>/cmdline`). The REST client sets the
header in memory; the push sends it in memory too (see below).

## Pushing from the host

The agent's workspace is a host directory bind-mounted into the container
(`-v /srv/agents/example/work:/work`), so the host can read the agent's
commits where they are and push them with the host's credential. The agent
commits as it does today; `github_push(repo, branch)` publishes the branch.

### The agent's repository is hostile

The agent's uid owns everything in the workspace, `.git` included, so a
git command run there by the service would read configuration the agent
wrote. Git configuration runs programs: `core.fsmonitor`, `core.hooksPath`
and hooks, `core.sshCommand`, `credential.helper`, `gpg.program`,
`core.pager`, filters (`filter.<x>.clean`/`smudge`, `diff.<x>.textconv`),
`include.path` pulling in any file, `url.<x>.insteadOf` redirecting the
remote, `objects/info/alternates` reaching objects outside the workspace.
Git's `safe.directory` refuses a repository owned by another user for this
reason; the service must never relax it.

So **the service never runs git in the agent's repository.** It reads the
repository as data, in process:

1. **Resolve the checkout.** The policy names the workspace and the
   checkout directory of each repository; the agent names neither. The
   service opens them with `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS |
   RESOLVE_NO_MAGICLINKS)` from the workspace's directory file
   descriptor, and every later file of the repository the same way from
   `.git`'s. `.git` must be a directory (a `gitdir:` file, as a linked
   worktree has, is refused in the first version). No configuration file
   is read at all: not the repository's, the user's or the system's.
2. **Name the branch.** `branch` must be a valid ref name
   (`git check-ref-format` rules, implemented, not run), at most 200
   bytes, of `[A-Za-z0-9._/-]`, with the agent's prefix. The service reads
   `refs/heads/<branch>`, or its line in `packed-refs`, once, and from then
   on works with that commit id only: whatever the agent does to the ref
   during the push does not matter.
3. **Read the objects.** Loose objects and pack files of `objects/`, by the
   id they claim, with every object's hash verified as it is read (an
   object is its hash, so a file the agent swaps in changes nothing it
   could not have pushed anyway). `objects/info/alternates`, a commit graph
   or a `shallow` file is refused (a shallow clone cannot prove what is
   new). Reads are bounded (`max_push_bytes`, `max_blob_bytes`, a limit on
   objects walked), so a crafted pack cannot exhaust memory or time.
4. **Copy what is new into a host repository.** The service keeps, per
   agent and repository, a bare repository of its own under
   `/var/lib/redoma-github/<name>/<owner>/<repo>.git`, fetched from GitHub
   with the host's credential (trusted contents, the service's own
   configuration). The commits reachable from the agent's tip and not from
   that mirror's refs are the push. Their objects are written into the
   mirror, as data.
5. **Check, then push from the mirror** (see "What a push checks"): to
   `refs/heads/<branch>` on GitHub, from the commit id read in step 2,
   without force. When the branch exists on GitHub, its current commit must
   be an ancestor of the new one (the service checks before sending, and
   GitHub's refusal of a non-fast-forward is the backstop); a branch that
   would be deleted or rewound is refused.

The push itself can be in process (a Go git library's transport, the
token as an in-memory header) or `git push` on the mirror, which the
service owns and the agent cannot write. The latter must run with a fixed
environment: `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
`HOME` the service's state directory, `GIT_TERMINAL_PROMPT=0`,
`core.hooksPath=/dev/null`, `core.fsmonitor=false`,
`protocol.allow=never` with `protocol.https.allow=always`, the remote URL
given in the command (never from configuration) as
`https://github.com/<owner>/<repo>.git`, and the token through
`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` as an
`http.extraHeader`, so it is in the process's environment (its own uid's)
and not its arguments. Every argument comes from the policy or is a
checked commit id or ref name; no agent string reaches an argument list
unchecked, and none reaches a shell, which is never used.

### What a push checks

On every new commit, not only the tip, because every commit of a pushed
branch is published:

- the author and committer are `commit_author`, and with
  `require_signoff` the message has a matching `Signed-off-by:` trailer;
- the commit touches no `protected_paths`, compared with each of its
  parents: an added, changed or deleted file, a rename from or to one, a
  mode change, a submodule (gitlink) or a symlink pointing into one;
- no merge commits, unless the policy allows them (a merge brings in
  commits the checks above would also have to see; they do see them, but
  a policy that wants linear branches says so);
- no blob over `max_blob_bytes`, no more than `max_commits` commits and
  `max_push_bytes` of objects;
- the commit and tree objects parse strictly (no duplicate tree entries,
  no `.git` or `.gitmodules` tricks in a path, as git's own `fsck` checks).

A refusal names the commit and the rule, so the agent can fix its branch
(amend the author, drop the commit that touched a workflow) and push again.

GitHub keeps its own guards, which this design does not replace: a push
that changes `.github/workflows` without the `workflows` permission is
refused by GitHub (the App never has it), and a repository ruleset can
restrict which branches the App may create or update (for example
`agent/**`). A host should set both.

### Concurrency and the workspace's ownership

Pushes of one agent and repository are serialized. The service needs read
access to the workspaces, which the agent's uid owns: either the workspace
is group-readable by `redoma-github` (and the agent's umask keeps it so),
or the launcher or hook hands the service a directory file descriptor. This
is an open question.

## Network

With the tools in place, an agent's egress allowlist no longer needs
`github.com` or `api.github.com` for its own repositories, and should not
have them: a container with no GitHub host has no direct path to GitHub,
whatever it holds. What still needs GitHub, and how to scope it:

- **Fetching its own repositories** (to rebase on a moved base, or to
  check a pull request's merged state) needs the repository, private ones
  included. The proposal: `redoma github` also serves its mirrors read-only
  over git's smart HTTP protocol (`upload-pack` only; `receive-pack`
  refused), on the same route, so the agent's `origin` is
  `http://127.0.0.1:7070/git/<owner>/<repo>.git`. The agent fetches what the
  host fetched; it never pushes there. git speaks HTTP/1.1, so that route
  needs the listener to accept it next to the relay's HTTP/2 (or a listener
  of its own). A first version may instead keep
  anonymous `github.com` egress for public repositories and leave private
  ones to a later one.
- **Public dependencies**: release archives and source tarballs a build
  downloads from `github.com`, which redirect to
  `objects.githubusercontent.com` (and similar), and `raw.githubusercontent.com`.
  A CONNECT tunnel is opaque, so Envoy cannot allow some GitHub paths and
  not others. Prefer what avoids GitHub (a Go module proxy, a Bazel
  registry and download mirror); where a host must allow GitHub hosts for
  downloads, the container still holds no credential, so what it can do
  there is what anyone anonymous can.
- **Nothing in the default allowlist** (`egress/_default.txt`) should name
  a GitHub host; an agent that needs one gets it in its own file.

## Audit

Every call is one structured log line (to the journal, and optionally a
file under `/var/lib/redoma-github/audit/`):

- time, the agent's SPIFFE ID, the task id the shim claims (marked
  unverified), the call id;
- the tool and repository, and the arguments that identify the target
  (pull request number, branch, comment id, the commit id pushed and the
  remote's previous one);
- a SHA-256 of the arguments in canonical form, so bodies need not be
  logged and a body found on GitHub can be matched to its call;
- the result: `ok`, `refused` with the rule, or `error` with GitHub's
  status; GitHub's `X-GitHub-Request-Id`; the URL of what was created;
- the credential's role (never the token) and the duration.

Refusals are logged like successes: a burst of them is what an agent
trying the policy's edges looks like.

## Rollout

1. **The service, read tools only**, behind `github_tools = on` in
   `redoma.conf` (off by default; off installs nothing in Envoy and the
   shim offers no github tools): `github_read_pr` and `github_list_prs`.
   The user and its id in `deb/sysusers.conf`, the hook's `ServiceIDs` and
   `postinst`; the SPIRE entry `spiffe://<td>/host/github` (`unix:uid:965`)
   in README.md; the route and cluster in `redoma xds`.
2. **Writes on GitHub's side**: `github_open_pr`, `github_update_pr`,
   `github_comment`, `github_reply`, the two credentials, rate limits.
3. **Push**: the in-process reader, the mirror, the checks.
4. **No GitHub egress**: the read-only git endpoint, then README.md's
   examples without GitHub hosts in the egress allowlist.

**Identity modes.** Nothing here depends on how `redoma xds` gets the
agents' SVIDs: in Delegated and Broker mode alike, Envoy presents the
agent's SVID and `redoma github` verifies it. The service's own SVID comes
from the Workload API, attested by uid, in both.

**Migrating an agent from an in-container `gh`:** write its policy file;
start its shim with `--github`; tell it (its instructions) to publish with
the tools; remove the token from its environment or credential helper and
the GitHub hosts from `egress/<name>.txt`; restart it. The policy is per
agent, so agents move one at a time, and an agent with no policy file is
unaffected.

**Tests**, under `bazel test //...`, with no network:

- the policy parser and every rule, table-driven, with the refusals'
  messages;
- the REST calls against a fake GitHub API (`httptest`), recording every
  request, so a test can assert that a refused call made none and that no
  call ever reaches a merge, review, status or workflow endpoint;
- the push against fixture repositories built in the test: a hook, an
  fsmonitor and a credential helper in `.git/config` that must never run
  (each would create a marker file), alternates, a symlinked ref, a
  `gitdir:` file, a shallow clone, a huge blob, a protected path changed in
  a middle commit and restored at the tip, a wrong author, a ref swapped
  during the push, a non-fast-forward; and a local bare repository as the
  remote;
- the mTLS identity checks, reusing `mtlstest`;
- the audit line of each outcome, and that no token appears in any log or
  error (a test greps them for the fake token).

An e2e test runs against a dedicated test repository with a test App, from
a workflow started by hand (it needs secrets and network): push a branch,
open a pull request, comment, reply, read it back, then the refusals (a
workflow file, a protected base, another agent's prefix), and close the
pull request and delete the branch with the workflow's own token, not the
tools.

## Open questions

1. **Workspace access.** Group-readable workspaces, a directory file
   descriptor passed by the hook or launcher, or the agent pushing a
   `git bundle` into a directory the service reads? The bundle avoids
   reading `.git` at all but moves the parsing to an agent-written file.
2. **Which git implementation** reads the agent's objects and pushes: a Go
   library in process (no git binary on the host's path, but its parser
   meets hostile input) or git on the mirror only (git's parser, but a
   process per push)? Either way, never git in the agent's repository.
3. **Same-repository branches run CI with the repository's secrets.** A
   pull request from a branch of the same repository runs its workflows
   with the base repository's secrets and a token that may write, and with
   the branch's own code (build scripts, tests). Protected paths stop
   changes to workflow files, not to what those workflows execute. Should
   the tools push to a fork the App owns and open the pull request across
   repositories, so CI treats it as a fork's? That costs the agent access
   to the base's CI results until a maintainer approves the run.
4. **Linked worktrees and submodules** in the agent's checkout: support
   them (a `gitdir:` file resolved beneath the workspace, submodule
   pushes) or refuse them, as the first version does?
5. **Commit signing.** Commits pushed this way are unsigned. Should the
   service sign them, which means rewriting them (new ids the agent must
   fetch back), or should a host that requires signed commits use the
   contents API for small changes instead?
6. **`write_scope = repo`.** Is commenting on any issue or pull request of
   a repository needed, or is `own` enough? Should reading be scoped the
   same way?
7. **Tool calls outside a turn.** The shim refuses them, but the service
   cannot tell. Should the relay tell `redoma github` which agents have an
   open task, so a call with none is refused?
8. **Uid.** 965 for `redoma-github`, next to 961, 963 and 964?
9. **The read-only git endpoint**: needed in the first version that drops
   GitHub egress, or does anonymous egress for public repositories cover
   the first hosts?
10. **Untrusted text**: is labelling authors enough, or should
    `trusted_authors` be required on public repositories?
11. **Other forges.** The tools are GitHub's; would a `forge` abstraction
    (GitLab, Gitea) be worth it now, or only when a host asks?
