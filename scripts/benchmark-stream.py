"""Measure actual SSE content arrival, excluding headers, role and reasoning.

Uses the local config by default; for a server set WEB2API_BASE_URL and
WEB2API_API_KEY. Output contains only timing/metadata, never credentials/prompts.
"""
import argparse
import json
import os
from pathlib import Path
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument('--models', nargs='+', default=['gemini-3.6-flash'])
parser.add_argument('--count', type=int, default=1)
parser.add_argument('--label', default='baseline')
parser.add_argument('--api', choices=['chat', 'responses', 'gemini'], default='chat')
parser.add_argument('--output', default='data/stream-benchmark.jsonl')
parser.add_argument('--prompt', default='用中文简要解释什么是流式 API，并列出三个优点。')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
config_path = root / 'data/local-config.json'
config = json.loads(config_path.read_text(encoding='utf-8-sig')) if config_path.exists() else {}
base = os.environ.get('WEB2API_BASE_URL', 'http://127.0.0.1:' + str(config.get('port', 8083)))
key = os.environ.get('WEB2API_API_KEY') or config.get('api_key')
if not key:
    key = (root / 'data/client-key.txt').read_text(encoding='utf-8-sig').strip()
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
for model in args.models:
    for run in range(args.count):
        payload = {'model': model, 'messages': [{'role': 'user', 'content': args.prompt}], 'stream': True}
        endpoint = '/v1/chat/completions'
        if args.api == 'responses':
            endpoint = '/v1/responses'
            payload = {'model': model, 'input': args.prompt, 'stream': True}
        elif args.api == 'gemini':
            endpoint = '/v1beta/models/' + model + ':streamGenerateContent?alt=sse'
            payload = {'contents': [{'role': 'user', 'parts': [{'text': args.prompt}]}]}
        req = urllib.request.Request(base.rstrip('/') + endpoint, data=json.dumps(payload).encode(),
                                     headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
        start = time.perf_counter()
        result = {'label': args.label, 'api': args.api, 'model': model, 'run': run + 1, 'first_content_ms': None,
                  'first_reasoning_ms': None, 'content_chunks': 0, 'content_chars': 0, 'done': False}
        try:
            with opener.open(req, timeout=200) as resp:
                result['http_status'] = resp.status
                result['headers_ms'] = round((time.perf_counter() - start) * 1000, 1)
                for line in resp:
                    if not line.startswith(b'data:'):
                        continue
                    data = line[5:].strip()
                    elapsed = round((time.perf_counter() - start) * 1000, 1)
                    if data == b'[DONE]':
                        result['done'] = True
                        break
                    event = json.loads(data)
                    if 'error' in event:
                        error = event['error']
                        result['error'] = {k: error.get(k) for k in ('code', 'type')} if isinstance(error, dict) else 'stream_error'
                    choices = event.get('choices', [])
                    if args.api == 'responses':
                        if event.get('type') == 'response.output_text.delta':
                            choices = [{'delta': {'content': event.get('delta', '')}}]
                        if event.get('type') == 'response.completed':
                            result['done'] = True
                    elif args.api == 'gemini':
                        choices = []
                        for candidate in event.get('candidates', []):
                            if candidate.get('finishReason'):
                                result['done'] = True
                            for part in candidate.get('content', {}).get('parts', []):
                                field = 'reasoning_content' if part.get('thought') else 'content'
                                choices.append({'delta': {field: part.get('text', '')}})
                    for choice in choices:
                        delta = choice.get('delta', {})
                        if delta.get('reasoning_content') and result['first_reasoning_ms'] is None:
                            result['first_reasoning_ms'] = elapsed
                        content = delta.get('content', '')
                        if content:
                            if result['first_content_ms'] is None:
                                result['first_content_ms'] = elapsed
                                print(json.dumps({'first_content_ms': elapsed, 'model': model}), flush=True)
                            result['content_chunks'] += 1
                            result['content_chars'] += len(content)
        except urllib.error.HTTPError as exc:
            result['http_status'] = exc.code
            # Never persist arbitrary upstream error bodies (they can include tokens).
            try:
                error = json.load(exc).get('error', {})
                result['error'] = {k: error.get(k) for k in ('code', 'type')}
            except (ValueError, AttributeError):
                result['error'] = 'http_error'
        except Exception as exc:
            result['error'] = type(exc).__name__ + ': ' + str(exc)
        result['total_ms'] = round((time.perf_counter() - start) * 1000, 1)
        print(json.dumps(result, ensure_ascii=True), flush=True)
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        with output.open('a', encoding='utf-8') as handle:
            handle.write(json.dumps(result, ensure_ascii=False) + '\n')
