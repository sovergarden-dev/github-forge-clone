import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';

export class HttpError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function sendJson(res: ServerResponse, status: number, value: unknown) {
  if (status === 204 || status === 202 && value === undefined) {
    res.writeHead(status);
    res.end();
    return;
  }
  const body = JSON.stringify(value);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(body),
  });
  res.end(body);
}

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const parts: Buffer[] = [];
  for await (const chunk of req) parts.push(chunk);
  return parts.length === 1 ? parts[0]! : Buffer.concat(parts);
}

async function dispatch(req: IncomingMessage, res: ServerResponse, token: string) {
  const url = new URL(req.url ?? '/', 'http://forge');
  const path = url.pathname;
  if (req.method === 'GET' && path === '/healthz') return sendJson(res, 200, { ok: true });
  if (/^\/(?:api(?:\/|$)|mcp(?:\/|$))/.test(path) || path.includes('.git/')) {
    if (!token || req.headers.authorization !== 'Bearer ' + token) {
      throw new HttpError(401, 'Bad credentials');
    }
  }
  if (req.method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) {
    res.setHeader('x-oauth-scopes', 'repo, read:org');
    return sendJson(res, 200, { current_user_url: '/api/v3/user', user_repositories_url: '/api/v3/user/repos' });
  }
  throw new HttpError(404, 'Not Found');
}

export function createApp() {
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  let requestId = 0;
  return createServer((req, res) => {
    res.sendDate = false;
    res.setHeader('x-github-request-id', (++requestId).toString(16));
    void dispatch(req, res, token).catch((error: unknown) => {
      if (res.destroyed || res.headersSent) return;
      const status = error instanceof HttpError ? error.status : 500;
      sendJson(res, status, { message: error instanceof HttpError ? error.message : 'Internal Server Error' });
    });
  });
}
