#!/usr/bin/env python3
"""Start the pinned Hindsight reference with the fully local evaluation profile."""
import argparse
import os
from pathlib import Path
import json
from dotenv import dotenv_values
from parity import verify_source, HINDSIGHT_SHA, require_loopback

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--hindsight-source',type=Path,required=True)
parser.add_argument('--env-file',type=Path,default=Path(__file__).with_name('local.env.example'))
parser.add_argument('--port',type=int,default=18889)
args=parser.parse_args()
verify_source(args.hindsight_source,HINDSIGHT_SHA)
env={k:v for k,v in os.environ.items() if not k.startswith(('AGENT_RUNTIME_MEMORY_','HINDSIGHT_API_')) and k not in {'OPENROUTER_API_KEY','OPENAI_API_KEY'}}
env.update({k:v for k,v in dotenv_values(args.env_file,interpolate=False).items() if v is not None})
url=env['AGENT_RUNTIME_MEMORY_MODEL_URL']
embedding_url=env.get('AGENT_RUNTIME_MEMORY_EMBEDDING_URL') or url
require_loopback(url);require_loopback(embedding_url)
options=json.loads(env['AGENT_RUNTIME_MEMORY_EXTRACTION_OPTIONS'])
env.update(HINDSIGHT_API_LLM_PROVIDER='openai',HINDSIGHT_API_LLM_BASE_URL=url,HINDSIGHT_API_LLM_MODEL=env['AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL'],HINDSIGHT_API_LLM_API_KEY='local',HINDSIGHT_API_LLM_EXTRA_BODY=json.dumps(options),HINDSIGHT_API_LLM_TEMPERATURE='none',HINDSIGHT_API_RETAIN_MAX_COMPLETION_TOKENS=str(options.get('max_tokens',8192)),HINDSIGHT_API_LLM_STRICT_SCHEMA='true',HINDSIGHT_API_EMBEDDINGS_PROVIDER='openai',HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=embedding_url,HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL=env['AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL'],HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY='local',HINDSIGHT_API_EMBEDDINGS_OPENAI_DIMENSIONS=env['AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS'],HINDSIGHT_API_RERANKER_PROVIDER='rrf',HINDSIGHT_API_ENABLE_OBSERVATIONS='false')
python=args.hindsight_source/'.venv/bin/python'
os.chdir(args.hindsight_source)
os.execvpe(str(python),[str(python),'-m','hindsight_api.main','--host','127.0.0.1','--port',str(args.port)],env)
