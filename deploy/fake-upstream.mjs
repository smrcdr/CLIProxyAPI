import http from 'node:http';
import { createHash } from 'node:crypto';

const requests = [];
let mode = 'normal';

const readBody = (req) => new Promise((resolve, reject) => {
  let raw = '';
  req.on('data', (chunk) => { raw += chunk; });
  req.on('end', () => {
    try { resolve(raw ? JSON.parse(raw) : {}); } catch (error) { reject(error); }
  });
  req.on('error', reject);
});

const json = (res, status, body) => {
  const raw = JSON.stringify(body);
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(raw) });
  res.end(raw);
};

const cacheLocations = (body) => {
  const found = [];
  (body.tools || []).forEach((block, index) => {
    if (block?.cache_control) found.push(`tools.${index}`);
  });
  (Array.isArray(body.system) ? body.system : []).forEach((block, index) => {
    if (block?.cache_control) found.push(`system.${index}`);
  });
  (body.messages || []).forEach((message, messageIndex) => {
    (Array.isArray(message?.content) ? message.content : []).forEach((block, blockIndex) => {
      if (block?.cache_control) found.push(`messages.${messageIndex}.content.${blockIndex}`);
    });
  });
  return found;
};

const systemTexts = (body) => {
  if (typeof body.system === 'string') return [body.system];
  return (Array.isArray(body.system) ? body.system : [])
    .map((block) => typeof block === 'string' ? block : block?.text)
    .filter(Boolean);
};

const contentTexts = (content) => {
  if (typeof content === 'string') return [content];
  return (Array.isArray(content) ? content : [])
    .map((block) => typeof block === 'string' ? block : block?.text)
    .filter(Boolean);
};

const keyLabel = (req) => {
  const key = String(req.headers['x-api-key'] || req.headers.authorization || 'missing');
  return createHash('sha256').update(key).digest('hex').slice(0, 10);
};

const anthropicUsage = {
  input_tokens: 11,
  cache_creation_input_tokens: 101,
  cache_read_input_tokens: 202,
  output_tokens: 3,
};

const streamResponse = (res, model) => {
  res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
  const event = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify(data)}\n\n`);
  event('message_start', {
    type: 'message_start',
    message: {
      id: 'msg_fake_stream', type: 'message', role: 'assistant', model,
      content: [], stop_reason: null,
      usage: { ...anthropicUsage, output_tokens: 0 },
    },
  });
  event('content_block_start', { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } });
  event('content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'FAKE_OK' } });
  event('content_block_stop', { type: 'content_block_stop', index: 0 });
  event('message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: 3 } });
  event('message_stop', { type: 'message_stop' });
  res.end();
};

const server = http.createServer(async (req, res) => {
  if (req.url === '/__reset' && req.method === 'POST') {
    requests.splice(0, requests.length);
    mode = 'normal';
    return json(res, 200, { ok: true });
  }
  if (req.url?.startsWith('/__mode') && req.method === 'POST') {
    const value = new URL(req.url, 'http://localhost').searchParams.get('value');
    if (!['normal', 'timeout-first-two', 'quota-first'].includes(value)) {
      return json(res, 400, { error: 'invalid mode' });
    }
    mode = value;
    requests.splice(0, requests.length);
    return json(res, 200, { ok: true, mode });
  }
  if (req.url === '/__summary' && req.method === 'GET') return json(res, 200, { requests });
  if (!req.url?.startsWith('/v1/messages') || req.method !== 'POST') {
    return json(res, 404, { error: { message: 'not found' } });
  }

  let body;
  try { body = await readBody(req); } catch { return json(res, 400, { error: { message: 'invalid json' } }); }
  const key = keyLabel(req);
  requests.push({
    key,
    model: body.model,
    stream: !!body.stream,
    cacheLocations: cacheLocations(body),
    systemTexts: systemTexts(body),
    messageRoles: (body.messages || []).map((message) => message?.role),
    messageTexts: (body.messages || []).map((message) => contentTexts(message?.content)),
  });

  if (mode === 'timeout-first-two' && requests.length <= 2) {
    return json(res, 500, { error: { type: 'InternalError', message: 'Request timed out' } });
  }
  if (mode === 'quota-first' && requests.length === 1) {
    return json(res, 429, { error: { type: 'GoUsageLimitError', message: 'Weekly usage limit reached' } });
  }
  if (body.stream) return streamResponse(res, body.model);
  return json(res, 200, {
    id: 'msg_fake', type: 'message', role: 'assistant', model: body.model,
    content: [{ type: 'text', text: 'FAKE_OK' }], stop_reason: 'end_turn', usage: anthropicUsage,
  });
});

server.listen(9000, '0.0.0.0', () => console.error('fake upstream on :9000'));
