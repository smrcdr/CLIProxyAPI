import { createHash } from 'node:crypto';
import http from 'node:http';

const requests = [];
const modes = new Map();

function json(response, status, body) {
  const raw = JSON.stringify(body);
  response.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(raw),
  });
  response.end(raw);
}

function readBody(request) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let bytes = 0;
    request.on('data', (chunk) => {
      bytes += chunk.length;
      if (bytes > 1024 * 1024) {
        reject(new Error('request too large'));
        request.destroy();
        return;
      }
      chunks.push(chunk);
    });
    request.on('end', () => {
      try {
        const raw = Buffer.concat(chunks).toString('utf8');
        resolve(raw ? JSON.parse(raw) : {});
      } catch (error) {
        reject(error);
      }
    });
    request.on('error', reject);
  });
}

function targetFor(request) {
  return String(request.headers['x-fake-target'] || 'unknown').slice(0, 40);
}

function safeRequestRecord(request, body, target) {
  const affinity = String(request.headers['x-smartapi-affinity-key'] || '');
  return {
    at: new Date().toISOString(),
    target,
    path: request.url,
    model: typeof body.model === 'string' ? body.model.slice(0, 100) : '',
    stream: body.stream === true,
    reasoning_effort:
      typeof body.reasoning_effort === 'string' ? body.reasoning_effort.slice(0, 30) : '',
    affinity_hash: affinity
      ? createHash('sha256').update(affinity).digest('hex').slice(0, 12)
      : '',
    authorization_present: Boolean(request.headers.authorization || request.headers['x-api-key']),
  };
}

function applyMode(request, response, target) {
  const mode = modes.get(target) || 'normal';
  if (mode === 'normal') return false;
  if (mode === 'close') {
    request.socket.destroy();
    return true;
  }
  if (mode === 'malformed') {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end('{"broken":');
    return true;
  }
  const status = Number(mode);
  if ([401, 403, 429, 500, 502, 503].includes(status)) {
    const headers = { 'content-type': 'application/json' };
    if (status === 429) headers['retry-after'] = '1';
    response.writeHead(status, headers);
    response.end(JSON.stringify({ error: { message: `fake ${status}`, type: 'fake_error' } }));
    return true;
  }
  return false;
}

function responseUsage() {
  return {
    input_tokens: 17,
    output_tokens: 5,
    total_tokens: 22,
    input_tokens_details: { cached_tokens: 3 },
  };
}

function anthropicUsage() {
  return {
    input_tokens: 17,
    cache_creation_input_tokens: 0,
    cache_read_input_tokens: 3,
    output_tokens: 5,
  };
}

function chatUsage() {
  return {
    prompt_tokens: 17,
    completion_tokens: 5,
    total_tokens: 22,
    prompt_tokens_details: { cached_tokens: 3 },
  };
}

function streamResponses(response, model) {
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-cache',
  });
  const event = (name, data) => {
    response.write(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`);
  };
  event('response.created', {
    type: 'response.created',
    response: { id: 'resp_fake_stream', object: 'response', model, status: 'in_progress' },
  });
  event('response.output_text.delta', {
    type: 'response.output_text.delta',
    item_id: 'msg_fake',
    output_index: 0,
    content_index: 0,
    delta: 'FAKE_OK',
  });
  event('response.completed', {
    type: 'response.completed',
    response: {
      id: 'resp_fake_stream',
      object: 'response',
      model,
      status: 'completed',
      output: [
        {
          id: 'msg_fake',
          type: 'message',
          role: 'assistant',
          content: [{ type: 'output_text', text: 'FAKE_OK', annotations: [] }],
        },
      ],
      usage: responseUsage(),
    },
  });
  response.end('data: [DONE]\n\n');
}

function streamChat(response, model) {
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-cache',
  });
  const chunk = (data) => response.write(`data: ${JSON.stringify(data)}\n\n`);
  chunk({
    id: 'chatcmpl_fake',
    object: 'chat.completion.chunk',
    model,
    choices: [{ index: 0, delta: { role: 'assistant' }, finish_reason: null }],
  });
  chunk({
    id: 'chatcmpl_fake',
    object: 'chat.completion.chunk',
    model,
    choices: [{ index: 0, delta: { content: 'FAKE_OK' }, finish_reason: null }],
  });
  chunk({
    id: 'chatcmpl_fake',
    object: 'chat.completion.chunk',
    model,
    choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
  });
  chunk({
    id: 'chatcmpl_fake',
    object: 'chat.completion.chunk',
    model,
    choices: [],
    usage: chatUsage(),
  });
  response.end('data: [DONE]\n\n');
}

function streamAnthropic(response, model) {
  response.writeHead(200, {
    'content-type': 'text/event-stream',
    'cache-control': 'no-cache',
  });
  const event = (name, data) => {
    response.write(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`);
  };
  event('message_start', {
    type: 'message_start',
    message: {
      id: 'msg_fake_stream',
      type: 'message',
      role: 'assistant',
      model,
      content: [],
      stop_reason: null,
      usage: { ...anthropicUsage(), output_tokens: 0 },
    },
  });
  event('content_block_start', {
    type: 'content_block_start',
    index: 0,
    content_block: { type: 'text', text: '' },
  });
  event('content_block_delta', {
    type: 'content_block_delta',
    index: 0,
    delta: { type: 'text_delta', text: 'FAKE_OK' },
  });
  event('content_block_stop', { type: 'content_block_stop', index: 0 });
  event('message_delta', {
    type: 'message_delta',
    delta: { stop_reason: 'end_turn' },
    usage: { output_tokens: 5 },
  });
  event('message_stop', { type: 'message_stop' });
  response.end();
}

function handleControl(request, response, url) {
  if (url.pathname === '/__reset' && request.method === 'POST') {
    requests.length = 0;
    modes.clear();
    json(response, 200, { ok: true });
    return true;
  }
  if (url.pathname === '/__mode' && request.method === 'POST') {
    const target = String(url.searchParams.get('target') || '').slice(0, 40);
    const mode = String(url.searchParams.get('value') || '');
    if (!target || !['normal', 'close', 'malformed', '401', '403', '429', '500', '502', '503'].includes(mode)) {
      json(response, 400, { error: 'invalid mode' });
      return true;
    }
    modes.set(target, mode);
    json(response, 200, { ok: true, target, mode });
    return true;
  }
  if (url.pathname === '/__summary' && request.method === 'GET') {
    json(response, 200, { requests, modes: Object.fromEntries(modes) });
    return true;
  }
  return false;
}

const server = http.createServer(async (request, response) => {
  const url = new URL(request.url, 'http://router-fake');
  if (url.pathname === '/healthz') {
    json(response, 200, { status: 'ok' });
    return;
  }
  if (handleControl(request, response, url)) return;
  if (request.method !== 'POST') {
    json(response, 404, { error: { message: 'not found' } });
    return;
  }

  let body;
  try {
    body = await readBody(request);
  } catch {
    json(response, 400, { error: { message: 'invalid json' } });
    return;
  }

  const target = targetFor(request);
  requests.push(safeRequestRecord(request, body, target));
  if (applyMode(request, response, target)) return;

  const model = typeof body.model === 'string' ? body.model : 'fake-model';
  if (url.pathname.endsWith('/images/generations')) {
    json(response, 200, {
      created: Math.floor(Date.now() / 1000),
      data: [{ b64_json: 'ZmFrZS1pbWFnZQ==' }],
      usage: { input_tokens: 17, output_tokens: 5, total_tokens: 22 },
    });
    return;
  }
  if (url.pathname.endsWith('/responses')) {
    if (body.stream) {
      streamResponses(response, model);
      return;
    }
    json(response, 200, {
      id: 'resp_fake',
      object: 'response',
      created_at: Math.floor(Date.now() / 1000),
      status: 'completed',
      model,
      output: [
        {
          id: 'msg_fake',
          type: 'message',
          role: 'assistant',
          content: [{ type: 'output_text', text: 'FAKE_OK', annotations: [] }],
        },
      ],
      usage: responseUsage(),
    });
    return;
  }
  if (url.pathname.endsWith('/chat/completions')) {
    if (body.stream) {
      streamChat(response, model);
      return;
    }
    json(response, 200, {
      id: 'chatcmpl_fake',
      object: 'chat.completion',
      model,
      choices: [
        {
          index: 0,
          message: { role: 'assistant', content: 'FAKE_OK' },
          finish_reason: 'stop',
        },
      ],
      usage: chatUsage(),
    });
    return;
  }
  if (url.pathname.endsWith('/messages')) {
    if (body.stream) {
      streamAnthropic(response, model);
      return;
    }
    json(response, 200, {
      id: 'msg_fake',
      type: 'message',
      role: 'assistant',
      model,
      content: [{ type: 'text', text: 'FAKE_OK' }],
      stop_reason: 'end_turn',
      usage: anthropicUsage(),
    });
    return;
  }
  json(response, 404, { error: { message: 'not found' } });
});

server.listen(9000, '0.0.0.0', () => {
  console.error('SmartRouter fake upstream listening on :9000');
});
