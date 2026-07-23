import http from 'node:http';

const head = 'b'.repeat(40);
const base = 'a'.repeat(40);
const blobBase = 'c'.repeat(40);
const blobHead = 'd'.repeat(40);
const pullRequest = {
  id: 501,
  node_id: 'PR_501',
  number: 42,
  html_url: 'https://github.com/acme/widgets/pull/42',
  title: 'Fixture pull request',
  body: 'Deterministic local browser fixture.',
  user: { id: 9, node_id: 'U_9', login: 'author' },
  state: 'open',
  merged: false,
  draft: false,
  updated_at: '2026-07-23T10:00:00Z',
  head: { sha: head, repo: { id: 77, node_id: 'R_77', full_name: 'acme/widgets' } },
  base: { sha: base, ref: 'main', repo: { id: 77, node_id: 'R_77', full_name: 'acme/widgets' } },
  labels: [{ name: 'fixture' }],
  requested_reviewers: [{ id: 7, node_id: 'U_7', login: 'reviewer' }]
};
const diff = `diff --git a/README.md b/README.md
index ${blobBase.slice(0, 7)}..${blobHead.slice(0, 7)} 100644
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-old
+new
`;

const json = (response, value) => {
  response.setHeader('Content-Type', 'application/json');
  response.end(JSON.stringify(value));
};

http.createServer((request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (request.method !== 'GET') { response.statusCode = 405; response.end(); return; }
  if (url.pathname === '/user') { json(response, { id: 7, node_id: 'U_7', login: 'reviewer' }); return; }
  if (url.pathname === '/search/issues') {
    json(response, { total_count: 1, incomplete_results: false, items: [{ number: 42, repository_url: 'http://127.0.0.1:18081/repos/acme/widgets', pull_request: {} }] });
    return;
  }
  if (url.pathname === '/repos/acme/widgets/pulls/42/files') {
    json(response, [{ filename: 'README.md', status: 'modified', sha: blobHead, patch: '@@ -1 +1 @@\n-old\n+new' }]);
    return;
  }
  if (url.pathname === '/repos/acme/widgets/pulls/42') {
    if ((request.headers.accept || '').includes('diff')) { response.setHeader('Content-Type', 'text/plain'); response.end(diff); return; }
    json(response, pullRequest);
    return;
  }
  if (url.pathname === `/repos/acme/widgets/git/trees/${base}`) { json(response, { truncated: false, tree: [{ path: 'README.md', mode: '100644', type: 'blob', sha: blobBase }] }); return; }
  if (url.pathname === `/repos/acme/widgets/git/trees/${head}`) { json(response, { truncated: false, tree: [{ path: 'README.md', mode: '100644', type: 'blob', sha: blobHead }] }); return; }
  response.statusCode = 404;
  json(response, { message: 'fixture endpoint not found' });
}).listen(18081, '127.0.0.1');
