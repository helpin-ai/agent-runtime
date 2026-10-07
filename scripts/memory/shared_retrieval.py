#!/usr/bin/env python3
"""Retrieval-only comparison: replay Go-extracted facts/vectors into pinned Hindsight.

This deliberately bypasses reference extraction/entity resolution. It isolates
ranking from inference variation and never establishes end-to-end parity.
"""
import argparse
import asyncio
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import struct
import subprocess
import tempfile
import time
import uuid
import re
from datetime import datetime
import urllib.error
from dotenv import dotenv_values
import asyncpg
from parity import HINDSIGHT_SHA, ROOT, verify_source, require_loopback, http


def dt(value):
    return datetime.fromisoformat(value.replace('Z', '+00:00')) if value else None


def embedding_text(fact, mentioned):
    # Fixture producer mirrors the pinned augment_texts_with_dates input.
    text = fact['text']
    start = fact.get('occurred_start')
    date = start or mentioned
    end = fact.get('occurred_end')
    if date:
        month = dt(date).strftime('%B %Y')
        text += f" (happened from {month} to {dt(end).strftime('%B %Y')})" if end and dt(end) != dt(start) else f" (happened in {month})"
    if fact.get('entities'):
        text += " [" + ', '.join(fact['entities']) + "]"
    return text


def vector_digest(vectors):
    h = hashlib.sha256()
    for vector in vectors:
        h.update(struct.pack('<I', len(vector)))
        for value in vector:
            h.update(struct.pack('<f', value))
    return h.hexdigest()


async def replay(args, scope, rows, documents, chunks, entities, links):
    schema = args.schema
    if not re.fullmatch('[a-z_][a-z0-9_]*', schema):
        raise ValueError('invalid isolated schema identifier')
    conn = await asyncpg.connect(args.database_url)
    bank = 'agent-runtime-shared-' + uuid.uuid4().hex
    ids = {row['id']: uuid.uuid4() for row in rows}
    entity_ids = {name: uuid.uuid4() for _, name in entities}
    try:
        async with conn.transaction():
            await conn.execute(f'INSERT INTO {schema}.banks(bank_id,name) VALUES($1,$2)', bank, 'shared retrieval diagnostic')
            for doc in documents:
                await conn.execute(f'INSERT INTO {schema}.documents(id,bank_id,original_text) VALUES($1,$2,$3)', doc['document_id'], bank, doc['content'])
            for chunk in chunks:
                await conn.execute(f'INSERT INTO {schema}.chunks(chunk_id,document_id,bank_id,chunk_index,chunk_text) VALUES($1,$2,$3,$4,$5)', chunk['id'], chunk['document_id'], bank, chunk['chunk_index'], chunk['text'])
            for row in rows:
                f = json.loads(row['payload'])
                signals = list(f.get('entities') or [])
                for date in [f.get('occurred_start'), f.get('occurred_end')]:
                    if date:
                        signals.append(dt(date).strftime('%B %d %Y').replace(' 0', ' '))
                await conn.execute(f'''INSERT INTO {schema}.memory_units(id,bank_id,document_id,text,embedding,context,event_date,occurred_start,occurred_end,mentioned_at,fact_type,metadata,chunk_id,text_signals)
                    VALUES($1,$2,$3,$4,$5::vector,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$14)''',
                    ids[row['id']], bank, f['document_id'], f['text'], row['embedding'], f.get('context',''),
                    dt(f.get('occurred_start') or f.get('mentioned_at')), dt(f.get('occurred_start')), dt(f.get('occurred_end')),
                    dt(f.get('mentioned_at')), f['type'], json.dumps(f.get('metadata') or {}), f.get('chunk_id'), ' '.join(signals))
            for name, identity in entity_ids.items():
                await conn.execute(f'INSERT INTO {schema}.entities(id,bank_id,canonical_name) VALUES($1,$2,$3)', identity, bank, name)
            for fact_id, name in entities:
                await conn.execute(f'INSERT INTO {schema}.unit_entities(unit_id,entity_id) VALUES($1,$2)', ids[fact_id], entity_ids[name])
            for link in links:
                await conn.execute(f'INSERT INTO {schema}.memory_links(from_unit_id,to_unit_id,link_type,weight,bank_id) VALUES($1,$2,$3,$4,$5)', ids[link['source_id']], ids[link['target_id']], link['kind'], link['weight'], bank)
            await conn.execute(f"UPDATE {schema}.memory_units SET search_vector=to_tsvector('english',coalesce(text,'') || ' ' || coalesce(context,'') || ' ' || coalesce(text_signals,'')) WHERE bank_id=$1", bank)
            actual = []
            for row in rows:
                value = await conn.fetchval(f'SELECT embedding::text FROM {schema}.memory_units WHERE id=$1 AND bank_id=$2', ids[row['id']], bank)
                actual.append(json.loads(value))
            expected = [json.loads(row['embedding']) for row in rows]
            if vector_digest(actual) != vector_digest(expected):
                raise ValueError('reference stored vectors differ from Go vectors')
        return bank, vector_digest(expected)
    finally:
        await conn.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--hindsight-source', type=Path, required=True)
    parser.add_argument('--sqlite', type=Path, required=True)
    parser.add_argument('--source-report', type=Path, required=True)
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--env-file', type=Path, default=Path(__file__).with_name('local.env.example'))
    parser.add_argument('--database-url', default='postgresql://postgres:postgres@127.0.0.1:15438/postgres')
    parser.add_argument('--schema', default='parity_local_nomic768')
    parser.add_argument('--reference-port', type=int, default=18890)
    args = parser.parse_args()
    verify_source(args.hindsight_source, HINDSIGHT_SHA)
    config = {k:v for k,v in dotenv_values(args.env_file, interpolate=False).items() if v is not None}
    require_loopback(config['AGENT_RUNTIME_MEMORY_MODEL_URL'])
    require_loopback(config.get('AGENT_RUNTIME_MEMORY_EMBEDDING_URL') or config['AGENT_RUNTIME_MEMORY_MODEL_URL'])
    from urllib.parse import urlparse
    if urlparse(args.database_url).hostname not in ('127.0.0.1','localhost','::1'):
        raise ValueError('shared replay is restricted to an isolated local PostgreSQL server')
    source = json.loads(args.source_report.read_text())
    local = next(report for report in source if report['backend'] == 'local')
    scope = local['scope']
    conn = sqlite3.connect('file:' + str(args.sqlite.resolve()) + '?mode=ro', uri=True)
    conn.row_factory = sqlite3.Row
    rows = list(conn.execute('SELECT * FROM memory_facts WHERE app_id=? AND bank_id=? ORDER BY row_id', (scope['app_id'],scope['bank_id'])))
    documents = list(conn.execute('SELECT * FROM memory_documents WHERE app_id=? AND bank_id=? AND forgotten=0', (scope['app_id'],scope['bank_id'])))
    chunks = list(conn.execute('SELECT * FROM memory_chunks WHERE app_id=? AND bank_id=?', (scope['app_id'],scope['bank_id'])))
    entities = list(conn.execute('SELECT e.fact_id,e.name FROM memory_entities e JOIN memory_facts f ON e.fact_id=f.id WHERE f.app_id=? AND f.bank_id=?', (scope['app_id'],scope['bank_id'])))
    links = list(conn.execute('SELECT l.* FROM memory_links l JOIN memory_facts f ON l.source_id=f.id WHERE f.app_id=? AND f.bank_id=?', (scope['app_id'],scope['bank_id'])))
    conn.close()
    if not rows:
        raise ValueError('shared fixture contains no facts')
    fixture = json.loads(args.fixture.read_text())
    fixture['embedding_preprocessing'] = 'hindsight-date-entities-v1'
    fixture['recorded_facts'] = {doc['document_id']:[] for doc in documents}
    fixture['recorded_embeddings'] = {}
    original_to_index = {}
    for row in rows:
        f = json.loads(row['payload'])
        kept = {key:f[key] for key in ('text','type','entities','where','occurred_start','occurred_end') if key in f}
        chunk = next(c for c in chunks if c['id'] == f['chunk_id'])
        kept['source_chunk'] = {'index':chunk['chunk_index'],'text':chunk['text']}
        original_to_index[row['id']] = len(fixture['recorded_facts'][f['document_id']])
        fixture['recorded_facts'][f['document_id']].append(kept)
        key = embedding_text(f, f['mentioned_at'])
        vector = json.loads(row['embedding'])
        if key in fixture['recorded_embeddings'] and fixture['recorded_embeddings'][key] != vector:
            raise ValueError('conflicting recorded embedding input')
        fixture['recorded_embeddings'][key] = vector
    by_id = {row['id']:json.loads(row['payload']) for row in rows}
    for link in links:
        if link['kind'] != 'semantic':
            source_fact = by_id[link['source_id']]
            target_fact = by_id[link['target_id']]
            if source_fact['document_id'] != target_fact['document_id']:
                raise ValueError('cross-document causal recording needs explicit support')
            fact = fixture['recorded_facts'][source_fact['document_id']][original_to_index[link['source_id']]]
            fact.setdefault('causal_relations', []).append({'target_index':original_to_index[link['target_id']],'relation_type':link['kind']})
    embedding_url = (config.get('AGENT_RUNTIME_MEMORY_EMBEDDING_URL') or config['AGENT_RUNTIME_MEMORY_MODEL_URL']).rstrip('/')
    for query in fixture['queries']:
        text = query['request']['query']
        response = http(embedding_url+'/embeddings', {'model':config['AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL'],'input':[text],'dimensions':int(config['AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS'])}, 'local')
        fixture['recorded_embeddings'][text] = response['data'][0]['embedding']
    env = {k:v for k,v in os.environ.items() if k not in ('OPENROUTER_API_KEY','OPENAI_API_KEY')}
    env.update(config)
    env['HINDSIGHT_API_DATABASE_URL'] = args.database_url
    env['HINDSIGHT_API_DATABASE_SCHEMA'] = args.schema
    with tempfile.TemporaryDirectory(prefix='shared-retrieval-') as tmp:
        tmp = Path(tmp)
        profile = tmp/'profile.env'
        profile.write_text('\n'.join(k+'='+json.dumps(v) for k,v in env.items() if k.startswith(('AGENT_RUNTIME_MEMORY_','HINDSIGHT_API_'))) + '\n')
        recorded = tmp/'fixture.json';recorded.write_text(json.dumps(fixture))
        log = args.output.with_suffix('.reference.log')
        with log.open('w') as output:
            python = args.hindsight_source/'.venv/bin/python'
            server = subprocess.Popen([str(python),str(ROOT/'scripts/memory/start_local_reference.py'),'--hindsight-source',str(args.hindsight_source),'--env-file',str(profile),'--port',str(args.reference_port)], stdout=output, stderr=output)
            try:
                base = f'http://127.0.0.1:{args.reference_port}'
                for _ in range(60):
                    if server.poll() is not None: raise ValueError('reference startup failed; inspect log')
                    try:
                        http(base+'/health')
                        if 'Uvicorn running on' in log.read_text():break
                    except (urllib.error.URLError, TimeoutError): pass
                    time.sleep(1)
                else: raise ValueError('reference startup timed out')
                bank,digest = asyncio.run(replay(args,scope,rows,documents,chunks,entities,links))
                go = json.loads(subprocess.check_output(['go','run','./cmd/memory-eval','-recorded','-fixture',str(recorded)],cwd=ROOT,text=True))[0]
                reference=[]
                for query in fixture['queries']:
                    request=query['request']
                    body={'query':request['query'],'types':request.get('types') or ['world','experience'],'budget':'high','max_tokens':request.get('max_tokens',4096)}
                    if request.get('query_timestamp'):body['query_timestamp']=request['query_timestamp']
                    raw=http(base+'/v1/default/banks/'+bank+'/memories/recall',body)
                    selected=raw.get('results',[])[:request.get('limit',20)]
                    retrieved=list(dict.fromkeys(f['document_id'] for f in selected))
                    expected=query['expected_documents']
                    reference.append({'name':query['name'],'expected_documents':expected,'retrieved_documents':retrieved,'evidence_recall':len(set(expected)&set(retrieved))/len(set(expected)) if expected else 0,'evidence':raw})
                report={'profile':'shared-input retrieval diagnostic; reference extraction/entity resolution bypassed','hindsight_commit':HINDSIGHT_SHA,'reference_deployment_verified':True,'full_hindsight_parity':False,'source_fact_count':len(rows),'source_link_count':len(links),'stored_fact_vector_sha256':digest,'reference_stored_vectors_verified':True,'query_embeddings':'same local Nomic model; Go uses captured vectors, reference recomputes queries','local_only':True,'local':go,'hindsight':{'bank_id':bank,'queries':reference}}
                args.output.write_text(json.dumps(report,indent=2)+'\n')
                print(json.dumps({'fact_count':len(rows),'vectors_verified':True,'local_recall':go['mean_evidence_recall'],'hindsight_recall':sum(q['evidence_recall'] for q in reference)/len(reference)}))
            finally:
                server.terminate()
                try:server.wait(timeout=15)
                except subprocess.TimeoutExpired:server.kill();server.wait()

if __name__=='__main__':main()
