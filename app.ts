import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { deflateSync, gunzipSync, inflateSync } from 'node:zlib';

type Kind = 'commit' | 'tree' | 'blob' | 'tag';
type Entry = { mode: string; name: string; oid: string };
type ObjectData = {
  kind: Kind;
  bytes: Buffer;
  entries?: Entry[];
  links?: string[];
  compressed?: Buffer;
};
const kinds = [undefined, 'commit', 'tree', 'blob', 'tag'] as const;
const zero = '0'.repeat(40);
const refHead = (branch: string) => 'refs/heads/' + branch;
const invalid = (message = 'Validation Failed'): never => { throw new HttpError(422, message); };
const missing = (): never => { throw new HttpError(404, 'Not Found'); };
const badGit = (): never => { throw new HttpError(400, 'Invalid git data'); };

function hash(type: Kind, bytes: Buffer) {
  return createHash('sha1').update(type + ' ' + bytes.length + '\0').update(bytes).digest('hex');
}

function treeEntries(obj: ObjectData): Entry[] {
  if (obj.kind !== 'tree') return badGit();
  if (obj.entries) return obj.entries;
  const entries: Entry[] = [];
  const data = obj.bytes;
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(32, pos);
    const nul = data.indexOf(0, space + 1);
    if (space < pos || nul <= space || nul + 21 > data.length) return badGit();
    entries.push({ mode: data.toString('ascii', pos, space), name: data.toString('utf8', space + 1, nul), oid: data.toString('hex', nul + 1, nul + 21) });
    pos = nul + 21;
  }
  return obj.entries = entries;
}

function links(obj: ObjectData): string[] {
  if (obj.links) return obj.links;
  if (obj.kind === 'blob') return obj.links = [];
  if (obj.kind === 'tree') return obj.links = treeEntries(obj).filter(e => e.mode !== '160000').map(e => e.oid);
  const end = obj.bytes.indexOf('\n\n');
  if (end < 0) return badGit();
  const header = obj.bytes.toString('utf8', 0, end);
  return obj.links = Array.from(header.matchAll(/^(?:tree|parent|object) ([a-f0-9]{40})$/gm), m => m[1]!);
}

function gitNameOk(ref: string) {
  return /^refs\/[^/]+\/.+/.test(ref) && !/[\x00-\x20\x7f~^:?*\[\\]|\.{2}|@\{|\/\/|\/\.|\.lock(?:\/|$)|[./]$/.test(ref);
}

class GitStore {
  objects = new Map<string, ObjectData>();
  refs = new Map<string, string>();
  defaultBranch = 'main';
  lastRefs?: { request: Buffer; response: Buffer };
  lastFetch?: { request: Buffer; response: Buffer };

  updateRef(ref: string, oid: string | null) {
    if (oid === null) this.refs.delete(ref);
    else this.refs.set(ref, oid);
    this.lastRefs = this.lastFetch = undefined;
  }

  put(kind: Kind, bytes: Buffer): string {
    const oid = hash(kind, bytes);
    if (!this.objects.has(oid)) this.objects.set(oid, { kind, bytes });
    return oid;
  }

  get(oid: string) {
    return this.objects.get(oid) ?? badGit();
  }

  resolve(ref: string): string | undefined {
    if (ref === 'HEAD') return this.refs.get(refHead(this.defaultBranch));
    return this.refs.get(ref) ?? this.refs.get(refHead(ref)) ?? this.refs.get('refs/tags/' + ref) ?? (this.objects.has(ref) ? ref : undefined);
  }

  peel(oid: string) {
    let obj = this.get(oid);
    while (obj.kind === 'tag') {
      oid = links(obj)[0]!;
      obj = this.get(oid);
    }
    return oid;
  }

  *history(oid: string): Generator<string> {
    const visited = new Set<string>();
    const queue = [oid];
    for (let i = 0; i < queue.length; ++i) {
      const current = queue[i]!;
      if (visited.has(current)) continue;
      visited.add(current);
      const obj = this.get(current);
      if (obj.kind !== 'commit') continue;
      yield current;
      queue.push(...links(obj).slice(1));
    }
  }

  ancestor(old: string, next: string) {
    for (const ancestor of this.history(next)) if (ancestor === old) return true;
    return false;
  }

  root(oid: string) {
    const obj = this.get(this.peel(oid));
    return obj.kind === 'tree' ? this.peel(oid) : links(obj)[0] ?? badGit();
  }

  flatten(oid?: string) {
    const files = new Map<string, Entry>();
    if (!oid) return files;
    const walk = (tree: string, prefix: string) => {
      for (const entry of treeEntries(this.get(tree))) {
        const path = prefix + entry.name;
        if (entry.mode === '40000') walk(entry.oid, path + '/');
        else files.set(path, entry);
      }
    };
    walk(this.root(oid), '');
    return files;
  }

  makeTree(files: Map<string, Entry>) {
    interface Directory { files: Entry[]; dirs: Map<string, Directory> }
    const directory = (): Directory => ({ files: [], dirs: new Map() });
    const root = directory();
    for (const [path, entry] of files) {
      const parts = path.split('/');
      let dir = root;
      for (const name of parts.slice(0, -1)) {
        if (!dir.dirs.has(name)) dir.dirs.set(name, directory());
        dir = dir.dirs.get(name)!;
      }
      dir.files.push({ ...entry, name: parts.at(-1)! });
    }
    const encode = (dir: Directory): string => {
      for (const [name, child] of dir.dirs) dir.files.push({ name, mode: '40000', oid: encode(child) });
      const order = (e: Entry) => Buffer.from(e.name + (e.mode === '40000' ? '/' : ''));
      dir.files.sort((a, b) => Buffer.compare(order(a), order(b)));
      return this.put('tree', Buffer.concat(dir.files.flatMap(e => [Buffer.from(e.mode + ' ' + e.name + '\0'), Buffer.from(e.oid, 'hex')])));
    };
    return encode(root);
  }
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  function octet() {
    if (offset >= delta.length) return badGit();
    return delta[offset++]!;
  }
  function integer() {
    let value = 0;
    for (let shift = 0; shift <= 28; shift += 7) {
      const byte = octet();
      value += (byte & 127) * 2 ** shift;
      if (byte < 128) return value;
    }
    return badGit();
  }
  if (integer() !== base.length) return badGit();
  const size = integer();
  if (size > 64 * 1024 * 1024) return badGit();
  const result = Buffer.allocUnsafe(size);
  let written = 0;
  while (offset < delta.length) {
    const op = octet();
    if (op === 0) return badGit();
    if (op < 128) {
      if (offset + op > delta.length || written + op > size) return badGit();
      delta.copy(result, written, offset, offset + op);
      offset += op;
      written += op;
    } else {
      let start = 0, length = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) start += octet() * 256 ** i;
      for (let i = 0; i < 3; i++) if (op & (16 << i)) length += octet() * 256 ** i;
      if (!length) length = 65536;
      if (start + length > base.length || written + length > size) return badGit();
      base.copy(result, written, start, start + length);
      written += length;
    }
  }
  return written === size ? result : badGit();
}

function unpack(git: GitStore, data: Buffer) {
  const end = data.length - 20;
  if (end < 12 || data.toString('ascii', 0, 4) !== 'PACK' || ![2, 3].includes(data.readUInt32BE(4))) return badGit();
  if (!createHash('sha1').update(data.subarray(0, end)).digest().equals(data.subarray(end))) return badGit();
  const byOffset = new Map<number, ObjectData>();
  const staged = new Map<string, ObjectData>();
  type Delta = { offset: number; base: number | string; bytes: Buffer };
  let pending: Delta[] = [];
  let pos = 12;
  const octet = () => pos < end ? data[pos++]! : badGit();
  function accept(offset: number, kind: Kind, bytes: Buffer) {
    const oid = hash(kind, bytes);
    const obj = git.objects.get(oid) ?? { kind, bytes };
    byOffset.set(offset, obj);
    staged.set(oid, obj);
  }
  function resolve(delta: Delta) {
    const key = delta.base;
    const base = typeof key === 'number' ? byOffset.get(key) : staged.get(key) ?? git.objects.get(key);
    if (!base) return false;
    accept(delta.offset, base.kind, applyDelta(base.bytes, delta.bytes));
    return true;
  }
  const count = data.readUInt32BE(8);
  for (let i = 0; i < count; i++) {
    const offset = pos;
    let byte = octet();
    const type = byte >> 4 & 7;
    let size = byte & 15;
    for (let shift = 4; byte & 128; shift += 7) {
      if (shift > 32) return badGit();
      byte = octet();
      size += (byte & 127) * 2 ** shift;
    }
    if (size > 64 * 1024 * 1024) return badGit();
    let base: string | number | undefined;
    if (type === 6) {
      byte = octet();
      let distance = byte & 127;
      while (byte & 128) {
        byte = octet();
        distance = (distance + 1) * 128 + (byte & 127);
        if (distance > offset) return badGit();
      }
      base = offset - distance;
    } else if (type === 7) {
      base = data.toString('hex', pos, pos + 20);
      pos += 20;
    } else if (type < 1 || type > 4) return badGit();
    const inflated = inflateSync(data.subarray(pos, end), {
      info: true, maxOutputLength: Math.max(size, 1), chunkSize: Math.max(64, Math.min(65536, size + 1)),
    }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    pos += inflated.engine.bytesWritten;
    if (inflated.buffer.length !== size) return badGit();
    if (base === undefined) accept(offset, kinds[type as 1 | 2 | 3 | 4], inflated.buffer);
    else {
      const delta = { offset, base, bytes: inflated.buffer };
      if (!resolve(delta)) pending.push(delta);
    }
  }
  if (pos !== end) return badGit();
  while (pending.length) {
    const rest = pending.filter(d => !resolve(d));
    if (rest.length === pending.length) return badGit();
    pending = rest;
  }
  for (const [, obj] of staged) {
    for (const oid of links(obj)) if (!staged.has(oid) && !git.objects.has(oid)) return badGit();
  }
  for (const [oid, obj] of staged) git.objects.set(oid, obj);
}

function packHeader(type: number, size: number) {
  let byte = type * 16 + (size & 15);
  size = Math.floor(size / 16);
  const header: number[] = [];
  while (size) {
    header.push(byte | 128);
    byte = size & 127;
    size = Math.floor(size / 128);
  }
  header.push(byte);
  return Buffer.from(header);
}

function offsetEncoding(distance: number) {
  const bytes = [distance & 127];
  while ((distance = Math.floor(distance / 128)) > 0) bytes.push(128 | (--distance & 127));
  return Buffer.from(bytes.reverse());
}

function blobDelta(base: Buffer, target: Buffer) {
  const limit = Math.min(base.length, target.length);
  let prefix = 0, suffix = 0;
  while (prefix < limit && base[prefix] === target[prefix]) prefix++;
  while (suffix < limit - prefix && base[base.length - 1 - suffix] === target[target.length - 1 - suffix]) suffix++;
  if (prefix + suffix < Math.max(96, target.length / 2)) return null;
  const instructions: Buffer[] = [];
  for (let n of [base.length, target.length]) {
    const bytes: number[] = [];
    while (n >= 128) { bytes.push((n & 127) | 128); n = Math.floor(n / 128); }
    instructions.push(Buffer.from([...bytes, n]));
  }
  function copy(start: number, size: number) {
    while (size) {
      const length = Math.min(size, 0xffffff);
      let op = 128;
      const args: number[] = [];
      for (let i = 0; i < 7; ++i) {
        const byte = i < 4 ? (start >>> (8 * i)) & 255 : (length >>> (8 * (i - 4))) & 255;
        if (byte) { op |= 1 << i; args.push(byte); }
      }
      instructions.push(Buffer.from([op, ...args]));
      start += length;
      size -= length;
    }
  }
  copy(0, prefix);
  const end = target.length - suffix;
  for (let pos = prefix; pos < end; pos += 127) {
    const part = target.subarray(pos, Math.min(end, pos + 127));
    instructions.push(Buffer.from([part.length]), part);
  }
  copy(base.length - suffix, suffix);
  return Buffer.concat(instructions);
}

function pack(git: GitStore, objects: Set<string>, allowOffset: boolean) {
  const header = Buffer.alloc(12);
  header.write('PACK');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.size, 8);
  const parts: Buffer[] = [header];
  let offset = header.length;
  let base: { oid: string; bytes: Buffer; offset: number } | undefined;
  for (const oid of objects) {
    const obj = git.get(oid);
    let compressed = obj.compressed ?? deflateSync(obj.bytes, { level: 1 });
    if (compressed.length <= 65536) obj.compressed = compressed;
    let prefix = packHeader(kinds.indexOf(obj.kind), obj.bytes.length);
    if (obj.kind === 'blob' && obj.bytes.length >= 256) {
      const delta = base && blobDelta(base.bytes, obj.bytes);
      if (base && delta) {
        const ptr = allowOffset ? offsetEncoding(offset - base.offset) : Buffer.from(base.oid, 'hex');
        const encoded = deflateSync(delta, { level: 1 });
        if (encoded.length + ptr.length < compressed.length) {
          prefix = Buffer.concat([packHeader(allowOffset ? 6 : 7, delta.length), ptr]);
          compressed = encoded;
        }
      }
      if ((prefix[0]! >> 4 & 7) === 3) base = { oid, bytes: obj.bytes, offset };
    }
    parts.push(prefix, compressed);
    offset += prefix.length + compressed.length;
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

const flush = Buffer.from('0000');
const delim = Buffer.from('0001');
const packet = (s: Buffer | string) => {
  const data = typeof s === 'string' ? Buffer.from(s) : s;
  return Buffer.concat([Buffer.from((data.length + 4).toString(16).padStart(4, '0')), data]);
};
const packets = (lines: string[]) => Buffer.concat([...lines.map(s => packet(s + '\n')), flush]);

function readPackets(data: Buffer, receive = false) {
  const lines: string[] = [];
  let pos = 0;
  while (pos < data.length) {
    const hex = data.toString('ascii', pos, pos + 4);
    if (!/^[a-f0-9]{4}$/i.test(hex)) return badGit();
    const len = parseInt(hex, 16);
    pos += 4;
    if (len === 0 && receive) break;
    if (len < 3) continue;
    if (len < 4 || pos + len - 4 > data.length) return badGit();
    lines.push(data.toString('utf8', pos, pos + len - 4).replace(/\n$/, ''));
    pos += len - 4;
  }
  return { lines, tail: data.subarray(pos) };
}

function band(data: Buffer) {
  const chunks: Buffer[] = [];
  for (let start = 0; start < data.length; start += 65515) {
    chunks.push(packet(Buffer.concat([Buffer.from([1]), data.subarray(start, start + 65515)])));
  }
  chunks.push(flush);
  return Buffer.concat(chunks);
}

function lsRefs(git: GitStore, args: string[]) {
  const refs: string[] = [];
  const prefixes = args.filter(s => s.startsWith('ref-prefix ')).map(s => s.slice(11));
  const include = (ref: string) => prefixes.length === 0 || prefixes.some(p => ref.startsWith(p));
  const head = refHead(git.defaultBranch);
  if (include('HEAD') && (git.refs.has(head) || args.includes('unborn'))) {
    refs.push((git.refs.get(head) ?? 'unborn') + ' HEAD' + (args.includes('symrefs') ? ' symref-target:' + head : ''));
  }
  for (const ref of [...git.refs.keys()].sort()) {
    if (!include(ref)) continue;
    const oid = git.refs.get(ref)!;
    const peeled = args.includes('peel') ? git.peel(oid) : oid;
    refs.push(oid + ' ' + ref + (peeled === oid ? '' : ' peeled:' + peeled));
  }
  return packets(refs);
}

const v2Advertisement = packets(['version 2', 'agent=forge/1', 'ls-refs=unborn', 'fetch=shallow', 'object-format=sha1']);

function advertise(git: GitStore, service: string, v2: boolean) {
  if (v2) return v2Advertisement;
  const head = refHead(git.defaultBranch);
  const upload = service === 'git-upload-pack';
  const capabilities = upload
    ? 'side-band-64k ofs-delta shallow symref=HEAD:' + head + ' agent=forge/1'
    : 'report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=forge/1';
  const refs: string[] = [];
  if (upload && git.refs.has(head)) refs.push(git.refs.get(head) + ' HEAD');
  for (const ref of [...git.refs.keys()].sort()) {
    const oid = git.refs.get(ref)!;
    refs.push(oid + ' ' + ref);
    if (upload && git.get(oid).kind === 'tag') refs.push(git.peel(oid) + ' ' + ref + '^{}');
  }
  if (!refs.length) refs.push(zero + ' capabilities^{}');
  refs[0] += '\0' + capabilities;
  return Buffer.concat([packets(['# service=' + service]), packets(refs)]);
}

function closure(git: GitStore, tips: string[], depth: number, stops: Set<string>) {
  const objects = new Set<string>();
  const boundary = new Set<string>();
  const commits: [string, number][] = [];
  function visit(oid: string, level: number) {
    if (objects.has(oid)) return;
    const obj = git.get(oid);
    if (obj.kind === 'commit') {
      commits.push([oid, level]);
      return;
    }
    objects.add(oid);
    for (const child of links(obj)) visit(child, level);
  }
  for (const oid of tips) visit(oid, 1);
  for (let i = 0; i < commits.length; i++) {
    const [oid, level] = commits[i]!;
    if (objects.has(oid)) continue;
    objects.add(oid);
    const [tree, ...parents] = links(git.get(oid));
    visit(tree!, level);
    if (level >= depth || stops.has(oid)) {
      if (parents.length) boundary.add(oid);
    } else {
      for (const parent of parents) commits.push([parent, level + 1]);
    }
  }
  return { objects, boundary };
}

function uploadPack(git: GitStore, data: Buffer, v2: boolean) {
  if (v2 && git.lastRefs?.request.equals(data)) return git.lastRefs.response;
  if (v2 && git.lastFetch?.request.equals(data)) return git.lastFetch.response;
  const { lines } = readPackets(data);
  if (v2 && lines.includes('command=ls-refs')) {
    const response = lsRefs(git, lines);
    if (data.length <= 8192 && response.length <= 65536) git.lastRefs = { request: data, response };
    return response;
  }
  if (v2 && !lines.includes('command=fetch')) return badGit();
  const args = (key: string) => lines.filter(s => s.startsWith(key + ' ')).map(s => s.split(' ')[1]!);
  const wants = args('want');
  if (!wants.length) return flush;
  const haves = args('have').filter(oid => git.objects.has(oid));
  const shallow = new Set(args('shallow'));
  const depth = Number(args('deepen')[0] ?? Infinity);
  if (!(depth > 0)) return badGit();
  const response: Buffer[] = [];
  if (v2 && !lines.includes('done')) {
    response.push(packet('acknowledgments\n'), packets(haves.length ? haves.map(oid => 'ACK ' + oid) : ['NAK']));
    return Buffer.concat(response);
  }
  const relative = Number.isFinite(depth) && lines.includes('deepen-relative') && shallow.size > 0;
  const keepBoundary = relative || !Number.isFinite(depth);
  const want = closure(git, wants, relative ? Infinity : depth, keepBoundary ? shallow : new Set());
  const unshallow: string[] = [];
  for (const oid of shallow) {
    if (!want.objects.has(oid)) continue;
    if (relative && want.boundary.delete(oid)) {
      const extension = closure(git, links(git.get(oid)).slice(1), depth, new Set());
      for (const object of extension.objects) want.objects.add(object);
      for (const boundary of extension.boundary) want.boundary.add(boundary);
      unshallow.push(oid);
    } else if (!keepBoundary && !want.boundary.has(oid)) unshallow.push(oid);
  }
  const have = closure(git, haves, Infinity, shallow);
  for (const oid of have.objects) want.objects.delete(oid);
  const changes = [...want.boundary].filter(oid => !shallow.has(oid)).map(oid => 'shallow ' + oid);
  for (const oid of unshallow) changes.push('unshallow ' + oid);
  if (changes.length || !v2 && Number.isFinite(depth)) {
    if (v2) response.push(packet('shallow-info\n'));
    response.push(...changes.map(line => packet(line + '\n')), v2 ? delim : flush);
  }
  if (!v2 && !lines.includes('done') && !haves.length && Number.isFinite(depth)) return Buffer.concat(response);
  response.push(packet(v2 ? 'packfile\n' : haves.length ? 'ACK ' + haves[0] + '\n' : 'NAK\n'));
  const body = pack(git, want.objects, lines.some(l => l.split(' ').includes('ofs-delta')));
  response.push(v2 || lines.some(l => l.includes('side-band-64k')) ? band(body) : body);
  const wire = Buffer.concat(response);
  if (v2 && data.length <= 8192 && wire.length <= 1024 * 1024) git.lastFetch = { request: data, response: wire };
  return wire;
}

function receivePack(git: GitStore, data: Buffer) {
  const { lines, tail } = readPackets(data, true);
  if (!lines.length) return flush;
  const [first, capabilities = ''] = lines[0]!.split('\0');
  lines[0] = first!;
  const commands = lines.map(line => {
    const [old, next, ref] = line.split(' ');
    if (!ref || !gitNameOk(ref) || ![old, next].every(id => /^[a-f0-9]{40}$/.test(id ?? ''))) return badGit();
    return { old: old!, next: next!, ref };
  });
  let unpacked = true;
  try {
    if (tail.length) unpack(git, tail);
  } catch {
    unpacked = false;
  }
  const report = [unpacked ? 'unpack ok' : 'unpack invalid pack'];
  for (const { old, next, ref } of commands) {
    let reason = '';
    if (!unpacked) reason = 'unpacker error';
    else if ((git.refs.get(ref) ?? zero) !== old) reason = 'stale info';
    else if (next !== zero && !git.objects.has(next)) reason = 'missing object';
    else if (next !== zero && ref.startsWith('refs/heads/') && git.get(next).kind !== 'commit') reason = 'not a commit';
    else if (old !== zero && next !== zero && old !== next && (!ref.startsWith('refs/heads/') || !git.ancestor(old, next))) reason = 'non-fast-forward';
    if (reason) report.push('ng ' + ref + ' ' + reason);
    else {
      git.updateRef(ref, next === zero ? null : next);
      report.push('ok ' + ref);
    }
  }
  if (!capabilities.split(' ').includes('report-status')) return Buffer.alloc(0);
  const body = packets(report);
  return capabilities.split(' ').includes('side-band-64k') ? band(body) : body;
}

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
  sendJsonText(res, status, JSON.stringify(value));
}

function sendJsonText(res: ServerResponse, status: number, body: string) {
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(body),
  });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    let first: Buffer | undefined, rest: Buffer[] | undefined;
    req.on('data', (chunk: Buffer) => {
      if (!first) first = chunk;
      else (rest ??= [first]).push(chunk);
    });
    req.once('error', reject);
    req.once('end', () => {
      const body = rest ? Buffer.concat(rest) : first ?? Buffer.alloc(0);
      if (req.headers['content-encoding'] !== 'gzip') return resolve(body);
      try { resolve(gunzipSync(body, { maxOutputLength: 128 * 1024 * 1024 })); }
      catch { reject(new HttpError(400, 'Invalid compressed request')); }
    });
  });
}

type Input = Record<string, unknown>;
const record = (v: unknown): v is Input => typeof v === 'object' && v !== null && !Array.isArray(v);
const string = (v: unknown, field: string) => typeof v === 'string' && v.length ? v : invalid('Missing or invalid ' + field);
const nodeId = (kind: string, id: number) => Buffer.from(kind + ':' + id).toString('base64');

type Account = { id: number; node_id: string; login: string; type: string };
type Label = { id: number; node_id: string; name: string; color: string; description: string };
type Issue = {
  id: number; node_id: string; number: number; title: string; body: string; state: 'open' | 'closed';
  user: Account; labels: Label[]; comments: number; created_at: string; updated_at: string; closed_at: string | null;
};
type Comment = { id: number; node_id: string; body: string; user: Account; created_at: string; updated_at: string };
type Pull = Issue & {
  head: { ref: string; sha: string }; base: { ref: string; sha: string };
  merged: boolean; merged_at: string | null; merge_commit_sha: string | null;
};
const sameEntry = (a?: Entry, b?: Entry) => a?.oid === b?.oid && a?.mode === b?.mode;
function validPath(path: string) {
  return path && path.split('/').every(s => s && s !== '.' && s !== '..' && s.toLowerCase() !== '.git' && !s.includes('\0'));
}
function checkPaths(files: Map<string, Entry>) {
  for (const path of files.keys()) {
    for (let i = path.indexOf('/'); i >= 0; i = path.indexOf('/', i + 1)) if (files.has(path.slice(0, i))) return false;
  }
  return true;
}
function commitData(repo: Repository, sha: string) {
  const obj = repo.get(sha);
  if (obj.kind !== 'commit') return missing();
  const text = obj.bytes.toString('utf8');
  const split = text.indexOf('\n\n');
  const message = text.slice(split + 2).replace(/\n$/, '');
  const [tree, ...parents] = links(obj);
  return { sha, commit: { message, tree: { sha: tree } }, parents: parents.map(sha => ({ sha })), message, tree: { sha: tree } };
}

function fileInfo(repo: Repository, entry: Entry, path: string, withData = false) {
  const type = entry.mode === '40000' ? 'dir' : entry.mode === '160000' ? 'submodule' : entry.mode === '120000' ? 'symlink' : 'file';
  const bytes = type === 'dir' || type === 'submodule' ? null : repo.get(entry.oid).bytes;
  return { type, name: entry.name, path, sha: entry.oid, size: bytes?.length ?? 0,
    ...(withData && bytes ? { encoding: 'base64', content: bytes.toString('base64') } : {}) };
}

type Hunk = { start: number; end: number; insert: string[] };
const linesOf = (b?: Buffer): string[] => b?.toString('utf8').match(/[^\n]*\n|[^\n]+$/g) ?? [];

function lineEdits(a: string[], b: string[]): Hunk[] {
  let lo = 0, ai = a.length, bi = b.length;
  while (lo < ai && lo < bi && a[lo] === b[lo]) lo++;
  while (ai > lo && bi > lo && a[ai - 1] === b[bi - 1]) { ai--; bi--; }
  if (lo === ai && lo === bi) return [];
  const width = bi - lo + 1, height = ai - lo + 1;
  if (width * height > 1000000) return [{ start: lo, end: ai, insert: b.slice(lo, bi) }];
  const lcs = new Uint32Array(width * height);
  for (let i = ai - 1; i >= lo; i--) for (let j = bi - 1; j >= lo; j--) {
    const k = (i - lo) * width + j - lo;
    lcs[k] = a[i] === b[j] ? 1 + lcs[k + width + 1]! : Math.max(lcs[k + width]!, lcs[k + 1]!);
  }
  const hunks: Hunk[] = [];
  let i = lo, j = lo, edit: Hunk | undefined;
  while (i < ai || j < bi) {
    if (i < ai && j < bi && a[i] === b[j]) { i++; j++; edit = undefined; continue; }
    if (!edit) { edit = { start: i, end: i, insert: [] }; hunks.push(edit); }
    const k = (i - lo) * width + j - lo;
    if (j < bi && (i === ai || lcs[k + 1]! > lcs[k + width]!)) edit.insert.push(b[j++]!);
    else edit.end = ++i;
  }
  return hunks;
}

function mergeText(repo: Repository, original?: Entry, left?: Entry, right?: Entry): Entry | undefined {
  if (!original || !left || !right || !/^100(644|755)$/.test(original.mode) || !/^100(644|755)$/.test(left.mode) || !/^100(644|755)$/.test(right.mode)) return;
  const buffers = [original, left, right].map(e => repo.get(e.oid).bytes);
  if (buffers.some(buf => buf.includes(0) || !Buffer.from(buf.toString('utf8')).equals(buf))) return;
  const [base, ours, theirs] = buffers.map(linesOf);
  const oursEdits = lineEdits(base!, ours!);
  const edits = [...oursEdits];
  for (const next of lineEdits(base!, theirs!)) {
    const overlaps = oursEdits.filter(e => e.start === next.start || e.start < next.end && next.start < e.end);
    if (overlaps.length) {
      if (overlaps.length === 1 && overlaps[0]!.start === next.start && overlaps[0]!.end === next.end && overlaps[0]!.insert.join('') === next.insert.join('')) continue;
      return;
    }
    edits.push(next);
  }
  const result: string[] = [];
  let cursor = 0;
  for (const edit of edits.sort((a,b) => a.start - b.start)) {
    result.push(base!.slice(cursor, edit.start).join(''), edit.insert.join(''));
    cursor = edit.end;
  }
  result.push(base!.slice(cursor).join(''));
  return { ...left, mode: left.mode === original.mode ? right.mode : left.mode, oid: repo.put('blob', Buffer.from(result.join(''))) };
}

async function jsonBody(req: IncomingMessage) {
  const body = await readBody(req);
  if (!body.length) return {};
  try { return JSON.parse(body.toString('utf8')) as unknown; }
  catch { throw new HttpError(400, 'Problems parsing JSON'); }
}

class Repository extends GitStore {
  id: number;
  name: string;
  description: string;
  private: boolean;
  nextNumber = 1;
  labels = new Map<string, Label>();
  issues = new Map<number, Issue>();
  comments = new Map<number, Comment[]>();
  pulls = new Map<number, Pull>();
  issueJson = new WeakMap<Issue, string>();

  json(issue: Issue) {
    let text = this.issueJson.get(issue);
    if (text === undefined) { text = JSON.stringify(issue); this.issueJson.set(issue, text); }
    return text;
  }
  constructor(id: number, input: Input) {
    super();
    this.id = id;
    this.name = string(input.name, 'name');
    this.description = typeof input.description === 'string' ? input.description : '';
    this.private = input.private === true;
  }

  issue(number: number) { return this.issues.get(number) ?? this.pulls.get(number) ?? missing(); }
  pull(number: number) {
    const p = this.pulls.get(number) ?? missing();
    if (!p.merged) {
      p.head.sha = this.refs.get(refHead(p.head.ref)) ?? p.head.sha;
      p.base.sha = this.refs.get(refHead(p.base.ref)) ?? p.base.sha;
    }
    return p;
  }

  diff(pull: Pull) {
    const baseHistory = new Set(this.history(pull.base.sha));
    let ancestor: string | undefined;
    for (const oid of this.history(pull.head.sha)) if (baseHistory.has(oid)) { ancestor = oid; break; }
    const base = this.flatten(ancestor), head = this.flatten(pull.head.sha);
    return [...new Set([...base.keys(), ...head.keys()])].sort()
      .filter(path => !sameEntry(base.get(path), head.get(path)))
      .map(path => ({ path, before: base.get(path), after: head.get(path) }));
  }

  files(pull: Pull) {
    return this.diff(pull).map(({ path, before, after }) => {
      const bytes = (e?: Entry) => e && e.mode !== '160000' ? this.get(e.oid).bytes : undefined;
      const a = bytes(before), b = bytes(after);
      let additions = 0, deletions = 0;
      if (!a?.includes(0) && !b?.includes(0)) {
        for (const edit of lineEdits(linesOf(a), linesOf(b))) { additions += edit.insert.length; deletions += edit.end - edit.start; }
      }
      return { filename: path, sha: (after ?? before)!.oid, status: !before ? 'added' : !after ? 'removed' : 'modified', additions, deletions, changes: additions + deletions };
    });
  }

  mergeTree(pull: Pull) {
    const files = this.flatten(pull.base.sha);
    for (const { path, before, after } of this.diff(pull)) {
      const current = files.get(path);
      if (sameEntry(current, after)) continue;
      if (!sameEntry(current, before)) {
        const merged = mergeText(this, before, current, after);
        if (!merged) return null;
        files.set(path, merged);
      } else if (after) files.set(path, after);
      else files.delete(path);
    }
    return checkPaths(files) ? files : null;
  }
}

class Forge {
  // The published trial provisions one bearer principal. A standalone installation can name its own.
  owner = process.env.FORGE_OWNER ?? 'adaiversen';
  repos = new Map<string, Repository>();
  nextId = 2;
  readonly viewer = { login: this.owner, id: 1, node_id: nodeId('User', 1), type: 'User' };
  readonly instant: Date;
  readonly iso: string;

  constructor() {
    const now = process.env.MAKEFUN_TRIAL_NOW ?? '0';
    this.instant = new Date(/^\d+$/.test(now) ? Number(now) : now);
    this.iso = this.instant.toISOString();
  }

  repository(owner: string, name: string) {
    if (owner.toLowerCase() !== this.owner.toLowerCase()) return missing();
    return this.repos.get(name.toLowerCase()) ?? missing();
  }

  describe(repo: Repository) {
    return { id: repo.id, node_id: nodeId('Repository', repo.id), name: repo.name, full_name: this.owner + '/' + repo.name, private: repo.private, owner: this.viewer, default_branch: repo.defaultBranch, description: repo.description, has_issues: true };
  }

  createRepo(input: Input) {
    const name = string(input.name, 'name');
    if (!/^[A-Za-z0-9_.-]{1,100}$/.test(name) || name === '.' || name === '..') return invalid('Invalid repository name');
    if (this.repos.has(name.toLowerCase())) return invalid('Repository already exists');
    const repo = new Repository(this.nextId++, input);
    this.repos.set(name.toLowerCase(), repo);
    if (input.auto_init === true) this.contentWrite(repo, 'README.md', { message: 'Initial commit', content: Buffer.from('# ' + name + '\n').toString('base64') }, false);
    return repo;
  }

  commit(repo: Repository, branch: string, files: Map<string, Entry>, message: string, parents: string[], input: Input = {}) {
    const identity = (who: unknown) => {
      const info = record(who) ? who : {};
      const clean = (s: unknown, fallback: string) => typeof s === 'string' && s ? s.replace(/[\r\n<>]/g, '') : fallback;
      return clean(info.name, this.owner) + ' <' + clean(info.email, this.owner + '@users.noreply.github.com') + '> ' + Math.floor(this.instant.getTime() / 1000) + ' +0000';
    };
    const raw = ['tree ' + repo.makeTree(files), ...parents.map(id => 'parent ' + id), 'author ' + identity(input.author), 'committer ' + identity(input.committer ?? input.author), '', message.replace(/\n+$/, ''), ''].join('\n');
    const sha = repo.put('commit', Buffer.from(raw));
    repo.updateRef(refHead(branch), sha);
    return commitData(repo, sha);
  }

  contentWrite(repo: Repository, path: string, data: Input, remove: boolean) {
    if (!validPath(path)) return invalid('Invalid path');
    const branch = string(data.branch ?? repo.defaultBranch, 'branch');
    const parent = repo.refs.get(refHead(branch));
    if (!parent && (repo.refs.size || branch !== repo.defaultBranch)) return missing();
    const files = repo.flatten(parent);
    const previous = files.get(path);
    const message = string(data.message, 'message');
    if (remove && !previous) return missing();
    if (previous && !data.sha) return invalid('A sha is required to replace this file');
    if (data.sha !== undefined && data.sha !== previous?.oid) throw new HttpError(409, 'File SHA does not match');
    let content = null;
    if (remove) files.delete(path);
    else {
      if (typeof data.content !== 'string' || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(data.content.replace(/\s/g,''))) return invalid('Invalid base64 content');
      const entry = { name: path.split('/').at(-1)!, mode: previous?.mode ?? '100644', oid: repo.put('blob', Buffer.from(data.content, 'base64')) };
      files.set(path, entry);
      if (!checkPaths(files)) return invalid('File and directory names overlap');
      content = fileInfo(repo, entry, path, true);
    }
    const commit = this.commit(repo, branch, files, message, parent ? [parent] : [], data);
    return { status: previous ? 200 : 201, body: { content, commit } };
  }

  labelsFor(repo: Repository, values: unknown) {
    if (!Array.isArray(values)) return invalid('Invalid labels');
    return Array.from(new Set(values.map(v => string(record(v) ? v.name : v, 'label').toLowerCase())))
      .map(name => repo.labels.get(name) ?? invalid('Unknown label'));
  }

  newIssue(repo: Repository, input: Input, kind = 'Issue'): Issue {
    const title = string(input.title, 'title');
    const labels = this.labelsFor(repo, input.labels ?? []);
    const id = this.nextId++;
    const issue: Issue = { id, node_id: nodeId(kind, id), number: repo.nextNumber++, title, body: typeof input.body === 'string' ? input.body : '', state: 'open', user: this.viewer, labels, comments: 0, created_at: this.iso, updated_at: this.iso, closed_at: null };
    if (kind === 'Issue') repo.issues.set(issue.number, issue);
    return issue;
  }

  editIssue(repo: Repository, issue: Issue, input: Input) {
    const title = input.title === undefined ? issue.title : string(input.title, 'title');
    const labels = input.labels === undefined ? issue.labels : this.labelsFor(repo, input.labels);
    if (input.state !== undefined && input.state !== 'open' && input.state !== 'closed') return invalid('Invalid state');
    Object.assign(issue, { title, labels, body: typeof input.body === 'string' ? input.body : issue.body, updated_at: this.iso });
    if (input.state === 'closed' || input.state === 'open') {
      issue.state = input.state;
      issue.closed_at = issue.state === 'closed' ? this.iso : null;
    }
    repo.issueJson.delete(issue);
    return issue;
  }

  comment(repo: Repository, issue: Issue, body: unknown) {
    const text = string(body, 'body');
    const id = this.nextId++;
    const comment: Comment = { id, node_id: nodeId('IssueComment', id), body: text, user: this.viewer, created_at: this.iso, updated_at: this.iso };
    const comments = repo.comments.get(issue.number) ?? [];
    comments.push(comment);
    repo.comments.set(issue.number, comments);
    issue.comments = comments.length;
    repo.issueJson.delete(issue);
    return comment;
  }

  openPull(repo: Repository, data: Input) {
    const branch = (value: unknown) => {
      const name = string(value, 'branch');
      const [owner, ref] = name.includes(':') ? name.split(':') : [this.owner, name];
      if (owner?.toLowerCase() !== this.owner.toLowerCase()) return invalid('Unknown owner');
      return { ref: ref!, sha: repo.refs.get(refHead(ref!)) ?? invalid('Unknown branch') };
    };
    const head = branch(data.head), base = branch(data.base);
    if (head.sha === base.sha || repo.ancestor(head.sha, base.sha)) return invalid('No commits between branches');
    if ([...repo.pulls.values()].some(p => p.state === 'open' && p.head.ref === head.ref && p.base.ref === base.ref)) return invalid('A pull request already exists');
    const p: Pull = { ...this.newIssue(repo, data, 'PullRequest'), head, base, merged: false, merge_commit_sha: null, merged_at: null };
    repo.pulls.set(p.number, p);
    return p;
  }

  merge(repo: Repository, number: number, input: Input = {}) {
    const p = repo.pull(number);
    if (p.state !== 'open') throw new HttpError(405, 'Pull request is closed');
    if (input.sha && input.sha !== p.head.sha) throw new HttpError(409, 'Head branch was modified');
    const files = repo.mergeTree(p);
    if (!files) throw new HttpError(405, 'Pull request is not mergeable');
    const parents = input.merge_method === 'merge' ? [p.base.sha, p.head.sha] : [p.base.sha];
    const message = String(input.commit_title ?? p.title) + '\n\n' + String(input.commit_message ?? p.body);
    const commit = this.commit(repo, p.base.ref, files, message, parents);
    Object.assign(p, { merged: true, state: 'closed', merged_at: this.iso, closed_at: this.iso, merge_commit_sha: commit.sha });
    repo.issueJson.delete(p);
    return { sha: commit.sha, merged: true, message: 'Pull Request successfully merged' };
  }
}

function page<T>(url: URL, res: ServerResponse, rows: T[]) {
  const size = Math.min(100, Math.max(1, Number(url.searchParams.get('per_page')) || 30));
  const number = Math.max(1, Number(url.searchParams.get('page')) || 1);
  const last = Math.ceil(rows.length / size);
  const links: string[] = [];
  const link = (n: number, rel: string) => {
    const next = new URL(url);
    next.searchParams.set('page', String(n));
    links.push('<' + next.href + '>; rel="' + rel + '"');
  };
  if (number < last) { link(number + 1, 'next'); link(last, 'last'); }
  if (number > 1) { link(number - 1, 'prev'); link(1, 'first'); }
  if (links.length) res.setHeader('link', links.join(', '));
  return rows.slice((number - 1) * size, number * size);
}

function listItems<T extends Issue>(items: Iterable<T>, state: string, asc: boolean) {
  return [...items].filter(i => state === 'all' || i.state === state).sort((a,b) => asc ? a.number - b.number : b.number - a.number);
}

function readContent(repo: Repository, path: string, ref: string) {
  const commit = repo.resolve(ref) ?? missing();
  let entry: Entry = { name: '', mode: '40000', oid: repo.root(commit) };
  for (const name of path ? path.split('/') : []) {
    if (entry.mode !== '40000') return missing();
    entry = treeEntries(repo.get(entry.oid)).find(e => e.name === name) ?? missing();
  }
  if (entry.mode !== '40000') return fileInfo(repo, entry, path, true);
  return treeEntries(repo.get(entry.oid)).map(e => fileInfo(repo, e, (path ? path + '/' : '') + e.name));
}

async function rest(req: IncomingMessage, res: ServerResponse, forge: Forge, url: URL) {
  const method = req.method;
  const parts = url.pathname.slice('/api/v3/'.length).replace(/\/$/, '').split('/').map(decodeURIComponent);
  const json = ['POST','PUT','PATCH','DELETE'].includes(method ?? '') ? await jsonBody(req) : {};
  if (!record(json)) return invalid();
  const ok = (body: unknown, status = 200) => sendJson(res, status, body);
  const query = url.searchParams;
  if (parts.length === 1 && method === 'GET') {
    if (parts[0] === 'user') return ok(forge.viewer);
    if (parts[0] === 'meta') return ok({ installed_version: '3.17.0' });
  }
  if (parts.length === 2 && parts[0] === 'user' && parts[1] === 'repos') {
    if (method === 'POST') return ok(forge.describe(forge.createRepo(json)), 201);
    if (method === 'GET') return ok(page(url, res, [...forge.repos.values()]).map(r => forge.describe(r)));
  }
  if (parts[0] !== 'repos' || !parts[1] || !parts[2]) return missing();
  const repo = forge.repository(parts[1], parts[2]);
  const [resource, identifier, action] = parts.slice(3);
  const path = parts.slice(4).join('/');
  if (parts.length === 3) {
    if (method === 'DELETE') { forge.repos.delete(repo.name.toLowerCase()); return ok(undefined, 204); }
    if (method === 'PATCH') {
      if (json.default_branch !== undefined) {
        const branch = string(json.default_branch, 'default_branch');
        if (!repo.refs.has(refHead(branch))) return invalid('Unknown default branch');
        repo.defaultBranch = branch;
        repo.lastRefs = repo.lastFetch = undefined;
      }
      if (typeof json.description === 'string') repo.description = json.description;
      if (typeof json.private === 'boolean') repo.private = json.private;
    }
    if (method === 'PATCH' || method === 'GET') return ok(forge.describe(repo));
  }
  if (resource === 'git' && (identifier === 'refs' || identifier === 'ref')) {
    const ref = method === 'POST' ? string(json.ref, 'ref') : 'refs/' + parts.slice(5).join('/');
    const format = (sha: string) => ({ ref, object: { type: repo.get(sha).kind, sha } });
    if (method === 'POST' && !action) {
      const sha = string(json.sha, 'sha');
      if (!gitNameOk(ref) || repo.refs.has(ref) || !repo.objects.has(sha) || ref.startsWith('refs/heads/') && repo.get(sha).kind !== 'commit') return invalid('Invalid reference');
      repo.updateRef(ref, sha);
      return ok(format(sha), 201);
    }
    if (method === 'GET' && action) return ok(format(repo.refs.get(ref) ?? missing()));
  }
  if (resource === 'branches' && method === 'GET') {
    const describe = (ref: string) => ({ name: ref.slice(11), commit: { sha: repo.refs.get(ref) ?? missing() }, protected: false });
    return ok(path ? describe(refHead(path)) : page(url, res, [...repo.refs.keys()].filter(r => r.startsWith('refs/heads/')).sort()).map(describe));
  }
  if (resource === 'commits' && method === 'GET') {
    const ref = path || query.get('sha') || repo.defaultBranch;
    const sha = repo.resolve(ref) ?? missing();
    return ok(path ? commitData(repo, repo.peel(sha)) : page(url, res, [...repo.history(repo.peel(sha))]).map(id => commitData(repo, id)));
  }
  if (resource === 'contents') {
    if (method === 'GET') return ok(readContent(repo, path, query.get('ref') ?? repo.defaultBranch));
    if (method === 'PUT' || method === 'DELETE') {
      const result = forge.contentWrite(repo, path, json, method === 'DELETE');
      return ok(result.body, result.status);
    }
  }
  if (resource === 'labels' && parts.length <= 5) {
    if (method === 'GET') return ok(identifier ? repo.labels.get(identifier.toLowerCase()) ?? missing() : page(url, res, [...repo.labels.values()]));
    if (method === 'POST' && !identifier) {
      const name = string(json.name, 'name');
      const color = string(json.color ?? 'ededed', 'color');
      if (!/^[0-9a-f]{6}$/i.test(color) || repo.labels.has(name.toLowerCase())) return invalid('Invalid or duplicate label');
      const id = forge.nextId++;
      const label = { id, node_id: nodeId('Label', id), name, color, description: String(json.description ?? '') };
      repo.labels.set(name.toLowerCase(), label);
      return ok(label, 201);
    }
  }
  const asc = query.get('direction') === 'asc', state = query.get('state') ?? 'open';
  if (resource === 'issues') {
    if (!identifier) {
      if (method === 'POST') return ok(forge.newIssue(repo, json), 201);
      if (method === 'GET') return ok(page(url, res, listItems([...repo.issues.values(), ...repo.pulls.values()], state, asc)));
    }
    const issue = repo.issue(Number(identifier));
    if (!action && method === 'GET') return ok(issue);
    if (!action && method === 'PATCH') return ok(forge.editIssue(repo, issue, json));
    if (action === 'comments' && parts.length === 6) {
      if (method === 'GET') return ok(page(url, res, repo.comments.get(issue.number) ?? []));
      if (method === 'POST') return ok(forge.comment(repo, issue, json.body), 201);
    }
  }
  if (resource === 'pulls') {
    if (!identifier) {
      if (method === 'POST') return ok(forge.openPull(repo, json), 201);
      if (method === 'GET') return ok(page(url, res, listItems(repo.pulls.values(), state, asc)).map(p => repo.pull(p.number)));
    }
    const p = repo.pull(Number(identifier));
    if (!action && method === 'GET') return ok(p);
    if (action === 'files' && parts.length === 6 && method === 'GET') return ok(page(url, res, repo.files(p)));
    if (action === 'merge' && parts.length === 6 && method === 'PUT') return ok(forge.merge(repo, p.number, json));
  }
  return missing();
}

type Selection = { name: string; alias: string; args: Input; fields: Selection[]; on?: string; fragment?: string };
type Operation = { name: string; type: string; fields: Selection[]; defaults: Input };
type Variable = { variable: string };
const graphError = (message: string): never => { throw new Error(message); };

function parseDocument(source: string) {
  const tokens: string[] = [];
  const lex = /\s+|,|#[^\r\n]*|\.\.\.|[!$():=@\[\]{}|]|"(?:\\.|[^"\\])*"|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?|[_A-Za-z][_0-9A-Za-z]*/gy;
  let offset = 0;
  while (offset < source.length) {
    const match = lex.exec(source);
    if (!match) return graphError('Invalid token at ' + offset);
    offset = lex.lastIndex;
    if (!/^[\s,#]/.test(match[0])) tokens.push(match[0]);
  }
  let pos = 0;
  const next = () => tokens[pos++] ?? graphError('Unexpected end of document');
  const accept = (token: string) => tokens[pos] === token && (++pos, true);
  const need = (token: string) => accept(token) || graphError('Expected ' + token);
  const name = () => { const s = next(); return /^[_A-Za-z]/.test(s) ? s : graphError('Expected a field name'); };
  function value(): unknown {
    if (accept('$')) return { variable: name() } satisfies Variable;
    if (accept('[')) {
      const list = [];
      while (!accept(']')) list.push(value());
      return list;
    }
    if (accept('{')) {
      const object: Input = Object.create(null);
      while (!accept('}')) { const key = name(); need(':'); object[key] = value(); }
      return object;
    }
    const token = next();
    if (token.startsWith('"')) return JSON.parse(token);
    if (/^[-0-9]/.test(token)) return Number(token);
    if (token === 'true' || token === 'false') return token === 'true';
    return token === 'null' ? null : token;
  }
  function args() {
    const data: Input = Object.create(null);
    if (accept('(')) while (!accept(')')) { const key = name(); need(':'); data[key] = value(); }
    return data;
  }
  const directives = (data: Input) => { while (accept('@')) { const key = name(); data['@' + key] = args().if; } };
  function fields(): Selection[] {
    need('{');
    const result: Selection[] = [];
    while (!accept('}')) {
      if (accept('...')) {
        const inline = accept('on');
        const target = name(), options: Input = {};
        directives(options);
        result.push({ name: '', alias: '', args: options, ...(inline ? { on: target } : { fragment: target }), fields: inline ? fields() : [] });
      } else {
        const alias = name();
        const key = accept(':') ? name() : alias;
        const data = args();
        directives(data);
        result.push({ name: key, alias, args: data, fields: tokens[pos] === '{' ? fields() : [] });
      }
    }
    return result;
  }
  const operations: Operation[] = [];
  const fragments = new Map<string, { on: string; fields: Selection[] }>();
  while (pos < tokens.length) {
    if (accept('fragment')) {
      const key = name(); need('on'); fragments.set(key, { on: name(), fields: fields() }); continue;
    }
    let type = 'query', label = '';
    const defaults: Input = Object.create(null);
    if (tokens[pos] !== '{') {
      type = name();
      if (type !== 'mutation' && type !== 'query') return graphError('Unsupported operation ' + type);
      if (tokens[pos] !== '(' && tokens[pos] !== '{') label = name();
      if (accept('(')) while (!accept(')')) {
        need('$'); const key = name(); need(':');
        while (accept('[')) { /* nested list type */ }
        name();
        while (accept('!') || accept(']')) { /* list and nullability modifiers */ }
        if (accept('=')) defaults[key] = value();
      }
    }
    operations.push({ type, name: label, defaults, fields: fields() });
  }
  return { operations, fragments };
}

const typeFields: Record<string, string[]> = {
  User: ['id', 'login', 'databaseId'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'description', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'isPrivate', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Issue: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'author', 'createdAt', 'updatedAt', 'closedAt', 'comments', 'labels'],
  PullRequest: ['id', 'databaseId', 'number', 'title', 'body', 'author', 'createdAt', 'updatedAt', 'closedAt', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'mergeable', 'state', 'merged', 'files', 'comments', 'labels'],
  IssueComment: ['id', 'databaseId', 'body', 'author', 'createdAt', 'updatedAt'],
};
const typeEnums: Record<string, string[]> = { SearchType: ['ISSUE', 'REPOSITORY', 'USER'], IssueState: ['OPEN', 'CLOSED'], PullRequestState: ['OPEN', 'CLOSED', 'MERGED'] };

function connection<T>(all: T[], args: Input) {
  const cursor = (index: number) => Buffer.from('cursor:' + index).toString('base64');
  const start = typeof args.after === 'string' ? Number(Buffer.from(args.after, 'base64').toString().match(/^cursor:(\d+)$/)?.[1] ?? -1) + 1 : 0;
  const count = Math.max(0, Math.min(100, Number(args.first ?? 30)));
  const nodes = all.slice(start, start + count);
  const edges = nodes.map((node, i) => ({ node, cursor: cursor(start + i) }));
  return { nodes, edges, totalCount: all.length, pageInfo: { hasPreviousPage: start > 0, hasNextPage: start + nodes.length < all.length, startCursor: edges[0]?.cursor ?? null, endCursor: edges.at(-1)?.cursor ?? null } };
}

function graph(forge: Forge, request: unknown) {
  try {
    if (!record(request) || typeof request.query !== 'string') return graphError('Expected a GraphQL query');
    const { operations, fragments } = parseDocument(request.query);
    const op = request.operationName ? operations.find(o => o.name === request.operationName) : operations.length === 1 ? operations[0] : null;
    if (!op) return graphError('An operation name is required for this document');
    const variables = { ...op.defaults, ...(record(request.variables) ? request.variables : {}) };
    function resolve(v: unknown): unknown {
      if (Array.isArray(v)) return v.map(resolve);
      if (!record(v)) return v;
      if (typeof v.variable === 'string' && Object.keys(v).length === 1) return variables[v.variable];
      return Object.fromEntries(Object.entries(v).map(([k, child]) => [k, resolve(child)]));
    }
    const user = { __typename: 'User', id: forge.viewer.node_id, databaseId: forge.viewer.id, login: forge.owner };
    const comment = (c: Comment) => ({ __typename: 'IssueComment', id: c.node_id, databaseId: c.id, body: c.body, author: user, createdAt: c.created_at, updatedAt: c.updated_at });
    function issue(r: Repository, i: Issue) {
      return { __typename: 'Issue', id: i.node_id, databaseId: i.id, number: i.number, title: i.title, state: i.state.toUpperCase(), body: i.body, author: user, createdAt: i.created_at, updatedAt: i.updated_at, closedAt: i.closed_at,
        comments: (a: Input) => connection((r.comments.get(i.number) ?? []).map(comment), a),
        labels: (a: Input) => connection(i.labels.map(l => ({ ...l, id: l.node_id })), a) };
    }
    function pull(r: Repository, p: Pull) {
      return { ...issue(r, p), __typename: 'PullRequest', headRefName: p.head.ref, baseRefName: p.base.ref, headRefOid: p.head.sha, baseRefOid: p.base.sha, merged: p.merged, state: p.merged ? 'MERGED' : p.state.toUpperCase(),
        mergeable: () => p.merged ? 'UNKNOWN' : r.mergeTree(p) ? 'MERGEABLE' : 'CONFLICTING',
        files: (a: Input) => connection(r.files(p).map(f => ({ path: f.filename, additions: f.additions, deletions: f.deletions, changeType: f.status.toUpperCase() })), a) };
    }
    function selected<T extends Issue>(items: Iterable<T>, a: Input) {
      const states = a.states === undefined ? null : new Set((Array.isArray(a.states) ? a.states : [a.states]).map(s => String(s).toLowerCase()));
      return listItems(items, 'all', record(a.orderBy) && a.orderBy.direction === 'ASC')
        .filter(i => !states || states.has('merged' in i && i.merged ? 'merged' : i.state));
    }
    function repository(r: Repository) {
      const head = r.refs.get(refHead(r.defaultBranch));
      return { __typename: 'Repository', id: nodeId('Repository', r.id), databaseId: r.id, name: r.name, nameWithOwner: forge.owner + '/' + r.name, description: r.description, owner: user, defaultBranchRef: head ? { name: r.defaultBranch, target: { oid: head } } : null, viewerPermission: 'ADMIN', hasIssuesEnabled: true, isPrivate: r.private,
        issue: (a: Input) => r.issues.has(Number(a.number)) ? issue(r, r.issues.get(Number(a.number))!) : null,
        pullRequest: (a: Input) => r.pulls.has(Number(a.number)) ? pull(r, r.pull(Number(a.number))) : null,
        issues: (a: Input) => connection(selected(r.issues.values(), a).map(i => issue(r, i)), a),
        pullRequests: (a: Input) => connection(selected(r.pulls.values(), a).map(p => pull(r, r.pull(p.number))), a) };
    }
    function findNode(id: unknown) {
      for (const r of forge.repos.values()) {
        if (nodeId('Repository', r.id) === id) return { r, value: repository(r) };
        for (const i of r.issues.values()) if (i.node_id === id) return { r, i, value: issue(r, i) };
        for (const p of r.pulls.values()) if (p.node_id === id) return { r, i: p, p, value: pull(r, r.pull(p.number)) };
      }
      return null;
    }
    const query = { __typename: 'Query', viewer: user,
      repository: (a: Input) => {
        const r = String(a.owner).toLowerCase() === forge.owner.toLowerCase() && forge.repos.get(String(a.name).toLowerCase());
        return r ? repository(r) : null;
      },
      node: (a: Input) => a.id === user.id ? user : findNode(a.id)?.value ?? null,
      __type: (a: Input) => {
        const name = String(a.name), names = (rows: string[]) => rows.map(name => ({ name }));
        if (Object.hasOwn(typeFields, name)) return { kind: 'OBJECT', name, fields: names(typeFields[name]!), enumValues: null };
        return Object.hasOwn(typeEnums, name) ? { kind: 'ENUM', name, enumValues: names(typeEnums[name]!), fields: null } : null;
      } };
    function mutation(method: string, a: Input) {
      const data = record(a.input) ? a.input : graphError('Missing input for ' + method);
      const target = findNode(data.repositoryId ?? data.issueId ?? data.pullRequestId ?? data.subjectId) ?? graphError('Unknown node ID');
      const { r } = target;
      let payload: Input;
      switch (method) {
        case 'createIssue': payload = { issue: issue(r, forge.newIssue(r, data)) }; break;
        case 'closeIssue':
        case 'reopenIssue': {
          const i = target.i ?? graphError('Expected issue ID');
          payload = { issue: issue(r, forge.editIssue(r, i, { state: method === 'closeIssue' ? 'closed' : 'open' })) }; break;
        }
        case 'addComment': {
          const i = target.i ?? graphError('Expected issue or pull request ID');
          payload = { commentEdge: { node: comment(forge.comment(r, i, data.body)) } }; break;
        }
        case 'createPullRequest': payload = { pullRequest: pull(r, forge.openPull(r, { ...data, base: data.baseRefName, head: data.headRefName })) }; break;
        case 'mergePullRequest': {
          const p = target.p ?? graphError('Expected pull request ID');
          forge.merge(r, p.number, { merge_method: String(data.mergeMethod ?? 'SQUASH').toLowerCase(), sha: data.expectedHeadOid });
          payload = { pullRequest: pull(r, p) }; break;
        }
        default: return graphError('Unknown mutation ' + method);
      }
      return { ...payload, clientMutationId: data.clientMutationId ?? null };
    }
    function project(value: unknown, fields: Selection[], seen: Set<string>): unknown {
      if (value == null || !fields.length) return value ?? null;
      if (Array.isArray(value)) return value.map(v => project(v, fields, seen));
      if (!record(value)) return graphError('Selection on a scalar field');
      const output: Input = Object.create(null);
      for (const f of fields) {
        const a = resolve(f.args) as Input;
        if (a['@skip'] === true || a['@include'] === false) continue;
        if (f.on || f.fragment) {
          const spec = f.fragment ? fragments.get(f.fragment) ?? graphError('Unknown fragment ' + f.fragment) : f;
          if (spec.on !== value.__typename && spec.on !== 'Node') continue;
          if (f.fragment && seen.has(f.fragment)) return graphError('Cyclic fragment ' + f.fragment);
          const active = new Set(seen);
          if (f.fragment) active.add(f.fragment);
          Object.assign(output, project(value, spec.fields, active));
          continue;
        }
        if (!Object.hasOwn(value, f.name)) return graphError('Cannot query field "' + f.name + '" on type "' + (value.__typename ?? 'Object') + '"');
        const entry = value[f.name];
        output[f.alias] = project(typeof entry === 'function' ? entry(a) : entry, f.fields, seen);
      }
      return output;
    }
    const mutations = ['createIssue','closeIssue','reopenIssue','addComment','createPullRequest','mergePullRequest'];
    const root = op.type === 'query' ? query : Object.fromEntries(mutations.map(m => [m, (a: Input) => mutation(m, a)]));
    return { data: project(root, op.fields, new Set()) };
  } catch (e) { return { errors: [{ message: e instanceof Error ? e.message : 'Invalid document' }] }; }
}

const rpcVersions = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const serverInfo = { name: 'git-forge', version: '1.0.0' };
const capabilities = { tools: { listChanged: false } };
const toolDefinition = (name: string, description: string, required: string[], optional: string[] = []) => ({
  name, description, inputSchema: { type: 'object', properties: Object.fromEntries([...required, ...optional].map(key => [key, {
    type: key === 'pull_number' ? 'integer' : key === 'private' ? 'boolean' : 'string',
    ...(key === 'state' ? { enum: ['open', 'closed', 'all'] } : {}),
  }])), required },
});
const tools = [
  toolDefinition('create_repo', 'Create a repository for the authenticated user.', ['name'], ['description', 'private']),
  toolDefinition('get_repo', 'Read repository metadata by owner and name.', ['owner','repo']),
  toolDefinition('create_issue', 'Open an issue in a repository.', ['owner','repo','title'], ['body']),
  toolDefinition('list_issues', 'List issues in a repository by state.', ['owner','repo'], ['state']),
  toolDefinition('create_pull_request', 'Open a pull request between two branches.', ['owner','repo','title','head','base'], ['body']),
  toolDefinition('merge_pull_request', 'Squash and merge an open pull request.', ['owner','repo','pull_number']),
];
const rpcError = (id: unknown, code: number, message: string) => ({ jsonrpc: '2.0', id, error: { code, message } });

function runTool(forge: Forge, name: string, input: Input) {
  if (name === 'create_repo') return forge.describe(forge.createRepo(input));
  const repo = forge.repository(string(input.owner, 'owner'), string(input.repo, 'repo'));
  switch (name) {
    case 'get_repo': return forge.describe(repo);
    case 'create_issue': return forge.newIssue(repo, input);
    case 'list_issues': return listItems(repo.issues.values(), String(input.state ?? 'open'), false);
    case 'create_pull_request': return forge.openPull(repo, input);
    case 'merge_pull_request': return forge.merge(repo, Number(input.pull_number));
    default: return invalid('Unknown tool');
  }
}

async function mcp(req: IncomingMessage, res: ServerResponse, forge: Forge) {
  let message: unknown;
  try { message = await jsonBody(req); }
  catch { return sendJson(res, 400, rpcError(null, -32700, 'Parse error')); }
  if (!record(message) || message.jsonrpc !== '2.0' || typeof message.method !== 'string') return sendJson(res, 400, rpcError(null, -32600, 'Invalid Request'));
  const { method, id } = message;
  if (id === undefined) return sendJson(res, 202, undefined);
  const args = record(message.params) ? message.params : {};
  const ok = (result: unknown) => sendJson(res, 200, { jsonrpc: '2.0', id, result });
  const fail = (code: number, error: string) => sendJson(res, 200, rpcError(id, code, error));
  if (method === 'initialize') return ok({ protocolVersion: rpcVersions.includes(String(args.protocolVersion)) ? args.protocolVersion : rpcVersions[0], capabilities, serverInfo });
  if (method === 'server/discover') return ok({ supportedVersions: rpcVersions, capabilities, serverInfo, _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo } });
  if (method === 'ping') return ok({});
  if (method === 'tools/list') return ok({ tools });
  if (method !== 'tools/call') return fail(-32601, 'Method not found');
  const def = tools.find(t => t.name === args.name);
  if (!def) return fail(-32602, 'Unknown tool ' + String(args.name));
  const input = args.arguments;
  if (!record(input) || def.inputSchema.required.some(key => !Object.hasOwn(input, key))) return fail(-32602, 'Missing required arguments');
  for (const [key, value] of Object.entries(input)) {
    const schema = def.inputSchema.properties[key];
    if (schema && (schema.type === 'integer' ? !Number.isInteger(value) : typeof value !== schema.type)) return fail(-32602, 'Invalid ' + key);
  }
  try {
    const result = runTool(forge, def.name, input);
    return ok({ content: [{ type: 'text', text: JSON.stringify(result) }], structuredContent: Array.isArray(result) ? { issues: result } : result, isError: false });
  } catch (error) {
    if (!(error instanceof HttpError)) throw error;
    return ok({ isError: true, content: [{ type: 'text', text: error.message }] });
  }
}

async function dispatch(req: IncomingMessage, res: ServerResponse, token: string, forge: Forge) {
  const target = req.url ?? '/';
  const authenticated = !!token && req.headers.authorization === token;
  if (authenticated && (req.method === 'GET' || req.method === 'POST')) {
    const hit = /^\/api\/v3\/repos\/([\w.-]+)\/([\w.-]+)\/issues(?:\/([1-9]\d*))?$/.exec(target);
    if (hit) {
      const repo = forge.repository(hit[1]!, hit[2]!);
      if (req.method === 'GET' && hit[3]) {
        const number = Number(hit[3]);
        const issue = repo.issues.get(number);
        if (issue) return sendJsonText(res, 200, repo.json(issue));
        return sendJson(res, 200, repo.pull(number));
      }
      if (req.method === 'POST' && !hit[3]) {
        const data = await jsonBody(req);
        if (!record(data)) return invalid();
        return sendJsonText(res, 201, repo.json(forge.newIssue(repo, data)));
      }
    }
  }
  const url = new URL(target, 'http://' + (req.headers.host ?? 'forge'));
  const path = url.pathname;
  if (req.method === 'GET' && path === '/healthz') return sendJson(res, 200, { ok: true });
  if (!authenticated && (/^\/(?:api(?:\/|$)|mcp(?:\/|$))/.test(path) || path.includes('.git/'))) throw new HttpError(401, 'Bad credentials');
  if (req.method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) {
    res.setHeader('x-oauth-scopes', 'repo, read:org');
    return sendJson(res, 200, { current_user_url: '/api/v3/user', user_repositories_url: '/api/v3/user/repos' });
  }
  const remote = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-(?:upload|receive)-pack)$/.exec(path);
  if (remote) {
    const repo = forge.repository(decodeURIComponent(remote[1]!), decodeURIComponent(remote[2]!));
    const discovery = remote[3] === 'info/refs';
    const service = discovery ? url.searchParams.get('service') : remote[3];
    if (!['git-upload-pack', 'git-receive-pack'].includes(service ?? '') || req.method !== (discovery ? 'GET' : 'POST')) return missing();
    const v2 = service === 'git-upload-pack' && req.headers['git-protocol']?.includes('version=2') === true;
    const body = discovery ? advertise(repo, service!, v2) : service === 'git-upload-pack'
      ? uploadPack(repo, await readBody(req), v2) : receivePack(repo, await readBody(req));
    res.writeHead(200, { 'content-type': 'application/x-' + service + (discovery ? '-advertisement' : '-result'), 'content-length': body.length, 'cache-control': 'no-cache' });
    res.end(body);
    return;
  }
  if (path.startsWith('/api/v3/')) return rest(req, res, forge, url);
  if (path === '/api/graphql' && req.method === 'POST') {
    const body = await jsonBody(req);
    return sendJson(res, 200, Array.isArray(body) ? body.map(document => graph(forge, document)) : graph(forge, body));
  }
  if (path === '/mcp' && req.method === 'POST') return mcp(req, res, forge);
  throw new HttpError(404, 'Not Found');
}

export function createApp() {
  const token = process.env.MAKEFUN_SHOP_TOKEN ? 'Bearer ' + process.env.MAKEFUN_SHOP_TOKEN : '';
  const forge = new Forge();
  let requestId = 0;
  return createServer((req, res) => {
    res.sendDate = false;
    res.setHeader('x-github-request-id', (++requestId).toString(16));
    void dispatch(req, res, token, forge).catch((error: unknown) => {
      if (res.destroyed || res.headersSent) return;
      const status = error instanceof HttpError ? error.status : 500;
      sendJson(res, status, { message: error instanceof HttpError ? error.message : 'Internal Server Error' });
    });
  });
}
