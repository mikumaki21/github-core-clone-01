// Entry HTTP server: router, auth, JSON helpers. Wiring only — the surfaces
// live in git.ts, rest.ts, graphql.ts and mcp.ts. Do NOT add surface logic
// here; add it in the surface's own file.
//
// Auth: every request except GET /healthz must carry
// `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` when that env var is set and
// non-empty. Missing or wrong token is 401 JSON {"message":"Bad credentials"}.
// When the env var is empty (local dev without a token) any request passes,
// so the QA runner must start the server with MAKEFUN_SHOP_TOKEN set to the
// same token it passes with --token. The git client sends the token when it
// has one, so git is gated too.
// Every reply carries a non-empty X-GitHub-Request-Id header and is
// application/json, never HTML. Unknown routes are 404 {"message":"Not Found"}.
// A malformed JSON body on a route that reads JSON is 400 {"message":...}.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { randomUUID } from 'node:crypto';
import { Store } from './store.ts';
import { handleGit } from './git.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

export interface Ctx {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  path: string;
  segments: string[];
  query: URLSearchParams;
  body: Buffer;
  bodyText: string;
  bodyJson: unknown;
  bodyParseError: boolean;
  store: Store;
  requestId: string;
}

export type Handler = (ctx: Ctx) => Promise<boolean>;

const JSON_TYPE = 'application/json; charset=utf-8';

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

export function sendRaw(res: ServerResponse, status: number, body: Buffer, contentType: string, headers: Readonly<Record<string, string>> = {}): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length, ...headers });
  res.end(body);
}

function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

function decodeSegments(path: string): string[] {
  return path.split('/').filter((s) => s !== '').map((s) => {
    try {
      return decodeURIComponent(s);
    } catch {
      return s;
    }
  });
}

export function createApp(): Server {
  const store = new Store();
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    const requestId = randomUUID();
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => {
      if (Buffer.concat(chunks).length + chunk.length <= 16 * 1024 * 1024) chunks.push(chunk);
    });
    req.on('end', () => {
      void (async () => {
        const body = Buffer.concat(chunks);
        const bodyText = body.toString('utf8');
        let bodyJson: unknown;
        let bodyParseError = false;
        if (bodyText.trim() !== '') {
          try {
            bodyJson = JSON.parse(bodyText);
          } catch {
            bodyParseError = true;
          }
        }
        const path = pathOf(req);
        const method = req.method ?? 'GET';
        const url = new URL(req.url ?? '/', 'http://entry');
        const ctx: Ctx = {
          req,
          res,
          method,
          path,
          segments: decodeSegments(path),
          query: url.searchParams,
          body,
          bodyText,
          bodyJson,
          bodyParseError,
          store,
          requestId,
        };
        const headers = { 'x-github-request-id': requestId };
        try {
          if (method === 'GET' && path === '/healthz') {
            sendJson(res, 200, { ok: true }, headers);
            return;
          }
          if (token !== '') {
            const auth = req.headers.authorization ?? '';
            if (auth !== `Bearer ${token}`) {
              sendJson(res, 401, { message: 'Bad credentials' }, headers);
              return;
            }
          }
          // Git smart HTTP: /<owner>/<repo>.git/...
          if (path.includes('.git/') || path.endsWith('.git')) {
            if (await handleGit(ctx)) return;
          }
          if (path === '/api/v3' || path === '/api/v3/' || path.startsWith('/api/v3/')) {
            if (await handleRest(ctx)) return;
          }
          if (path === '/api/graphql') {
            if (await handleGraphql(ctx)) return;
          }
          if (path === '/mcp') {
            if (await handleMcp(ctx)) return;
          }
          sendJson(res, 404, { message: 'Not Found' }, headers);
        } catch (err) {
          if (!res.writableEnded) {
            // A malformed git request is a client error, not a server crash:
            // answer 400 and stay up, so the suite's error-survival checks pass.
            if (path.includes('.git')) {
              sendRaw(res, 400, Buffer.from('bad request\n', 'utf8'), 'text/plain; charset=utf-8', headers);
            } else {
              sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' }, headers);
            }
          }
        }
      })();
    });
  });
}
