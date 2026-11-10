"""Bounded Argon low-thinking input recall and long output checks. No config changes."""
import json
import pathlib
import random
import re
import runpy
import time
import uuid
from datetime import datetime

import requests

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT = ROOT / 'data' / ('argon-long-text-' + datetime.now().strftime('%Y%m%d-%H%M%S'))
OUT.mkdir(parents=True)
helper = runpy.run_path(str(ROOT / 'data/validate-egress-fairness-20261110.py'))
before = helper['snapshot']()
with helper['db']() as db:
    key = db.execute("select v from kv where k='api_key'").fetchone()[0]
    last_id = db.execute('select coalesce(max(id),0) from requests').fetchone()[0]
log_offset = (ROOT / 'data/stderr.log').stat().st_size
base = helper['BASE']
results = []


def save(name, value):
    (OUT / name).write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding='utf-8')


def emit(value):
    print(json.dumps(value, ensure_ascii=True), flush=True)


def request(name, prompt, expected=None):
    (OUT / (name + '-input.txt')).write_text(prompt, encoding='utf-8')
    row = dict(name=name, input_chars=len(prompt), input_utf8_bytes=len(prompt.encode()),
               first_text_s=None, done=False, finish_reason=None)
    started = time.monotonic()
    parts = []
    try:
        with requests.Session() as session:
            session.trust_env = False
            with session.post(base + '/v1/chat/completions',
                              headers={'Authorization': 'Bearer ' + key},
                              json={'model': 'gemini-4-argon', 'stream': True,
                                    'messages': [{'role': 'user', 'content': prompt}]},
                              stream=True, timeout=(10, 180)) as response:
                row['http_status'] = response.status_code
                if response.status_code != 200:
                    row['error'] = str(response.json().get('error', 'HTTP error')).replace(key, '[REDACTED]')[:500]
                else:
                    for line in response.iter_lines(chunk_size=1024):
                        if time.monotonic() - started > 600:
                            raise TimeoutError('600 second total limit')
                        if not line.startswith(b'data:'):
                            continue
                        payload = line[5:].strip()
                        if payload == b'[DONE]':
                            row['done'] = True
                            break
                        event = json.loads(payload)
                        if event.get('error'):
                            row['error'] = str(event['error']).replace(key, '[REDACTED]')[:500]
                        for choice in event.get('choices', []):
                            text = choice.get('delta', {}).get('content') or ''
                            if text:
                                if row['first_text_s'] is None:
                                    row['first_text_s'] = round(time.monotonic() - started, 3)
                                parts.append(text)
                            if choice.get('finish_reason'):
                                row['finish_reason'] = choice['finish_reason']
    except Exception as exc:
        row['error'] = type(exc).__name__ + ': ' + str(exc).replace(key, '[REDACTED]')[:200]
    text = ''.join(parts)
    (OUT / (name + '-output.txt')).write_text(text, encoding='utf-8')
    row.update(seconds=round(time.monotonic() - started, 3), output_chars=len(text),
               output_chinese_chars=len(re.findall(r'[\u4e00-\u9fff]', text)))
    row['transport_complete'] = row.get('http_status') == 200 and row['done'] and 'error' not in row
    if expected:
        try:
            parsed = json.loads(text.strip().removeprefix('```json').removeprefix('```').removesuffix('```').strip())
        except (ValueError, TypeError):
            parsed = {}
        row['recall'] = {k: parsed.get(k) == v for k, v in expected.items()}
        row['expected'] = expected
        row['ok'] = row['transport_complete'] and all(row['recall'].values())
    else:
        sections = re.findall(r'^SECTION_(\d{2})\s*$', text, flags=re.M)
        row['section_count'] = len(sections)
        row['section_order_ok'] = sections == [f'{i:02d}' for i in range(1, 61)]
        row['end_marker'] = text.rstrip().endswith('LONG_OUTPUT_END')
        row['length_met'] = row['output_chinese_chars'] >= 10000
        row['ok'] = row['transport_complete'] and row['section_order_ok'] and row['end_marker'] and row['length_met']
    results.append(row)
    save('results.json', results)
    emit(row)
    return row


emit({'output': str(OUT), 'model': 'gemini-4-argon', 'concurrency': 1, 'requests': 4})
for target in [10000, 50000, 100000]:
    rng = random.Random(target)
    lines = []
    size = 0
    while size < target:
        n = len(lines) + 1
        line = (f'档案{n:05d}：项目编号R{rng.randrange(100000,999999)}，本期完成{rng.randrange(20,999)}项核验，'
                f'库存批次{rng.randrange(1000,9999)}，预计下周处理{rng.randrange(20,999)}条记录。'
                '现场记录包括设备维护、仓储盘点、交接确认和质量抽查，所有待办均按既定流程登记。\n')
        lines.append(line)
        size += len(line)
    doc = ''.join(lines)[:target]
    expected = {k: uuid.uuid4().hex[:16] for k in ['begin', 'middle', 'end']}
    for fraction, field in [(0.9, 'end'), (0.5, 'middle'), (0.1, 'begin')]:
        pos = int(target * fraction)
        doc = doc[:pos] + f'\n唯一核验记录：字段={field}；值={expected[field]}。\n' + doc[pos:]
    prompt = ('阅读下列合成档案，找出三条“唯一核验记录”，仅返回JSON对象，字段名为begin、middle、end，'
              '值必须逐字复制相应记录中的值，不要解释或输出代码围栏。\n<档案>\n' + doc + '\n</档案>')
    row = request('input-' + str(target), prompt, expected)
    if row.get('http_status') == 429 or 'egress_cooldown' in row.get('error', ''):
        break
else:
    request('long-output',
            '请用中文撰写一份面向小型软件团队的接口服务运行手册。必须完整写出60节，每节标题单独一行，'
            '严格从SECTION_01连续编号到SECTION_60。每节正文至少180个汉字，总正文至少10000个汉字，'
            '每节讲不同的实际操作主题，包含步骤、检查依据和一个具体例子，不要重复段落，不要用省略号，'
            '不要以提纲替代正文，不要中途询问是否继续。最后一行必须是LONG_OUTPUT_END。')

after = helper['snapshot']()
with helper['db']() as db:
    rows = [dict(r) for r in db.execute('select id,model,account_id,status,error,total_ms from requests where id>? order by id', (last_id,))]
with (ROOT / 'data/stderr.log').open('rb') as log:
    log.seek(log_offset)
    lines = log.read().decode('utf-8', errors='replace').splitlines()
latency = [line for line in lines if '[latency]' in line and ('Argon' in line or 'trace=' in line)]
(OUT / 'latency.log').write_text('\n'.join(latency), encoding='utf-8')
save('verification.json', dict(server_rows=rows, runtime_unchanged=before['runtime'] == after['runtime'],
                               egress_backoff=after['egress_backoff'], strict_model=after['runtime'].get('strict_model')))
emit({'finished': True, 'requests': len(results), 'passed': sum(r['ok'] for r in results), 'output': str(OUT)})
