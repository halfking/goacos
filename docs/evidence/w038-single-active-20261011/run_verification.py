#!/usr/bin/env python3
"""Owned local replay. Private synthetic DSNs stay in subprocess memory."""
import os,json,subprocess,time,hashlib,pathlib,sys
ROOT=pathlib.Path(__file__).resolve().parents[3]
E=ROOT/'docs/evidence/w038-single-active-20261011'
GO='/tmp/memora-w020-toolchain-20261011/go-cache/bin/go'
SCAN='/tmp/memora-w020-dependency-tools-patched-20261011/govulncheck'
p=json.load(open('/tmp/goacos-w038-single-active-pg-20261011.json'))
m=json.load(open('/tmp/goacos-w038-single-active-mysql-20261011.json'))
env=dict(os.environ,GOTOOLCHAIN='local',GOCACHE='/tmp/goacos-single-active-gocache-20261011',GOACOS_TEST_PG_DSN=p['dsn'],GOACOS_TEST_MYSQL_DSN=m['dsn'])
results=[]
def run(name,args,extra=None):
 start=time.time();r=subprocess.run(args,cwd=ROOT,env=dict(env,**(extra or {})),text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
 for secret in (p['dsn'],m['dsn']):
  if secret in r.stdout:raise RuntimeError('private DSN appeared in output; report not saved')
 (E/(name+'.log')).write_text(r.stdout)
 results.append(dict(name=name,argv=args,environment_overrides=extra or {},exit_code=r.returncode,duration_seconds=round(time.time()-start,3)))
 (E/'verification.json').write_text(json.dumps({'results':results,'environment':{'GOTOOLCHAIN':'local','GOCACHE':env['GOCACHE'],'GOACOS_TEST_PG_DSN':'owned synthetic loopback PG17, parsed private file only in memory','GOACOS_TEST_MYSQL_DSN':'owned synthetic loopback MySQL8.4, parsed private file only in memory'}},indent=2)+'\n')
 print(name,'exit',r.returncode,flush=True)
 if r.returncode:print(r.stdout[-3000:]);sys.exit(r.returncode)
 return r.stdout
run('toolchain',[GO,'version'])
run('final-unit',[GO,'test','./...','-count=1'])
run('final-covered-race',[GO,'test','-race','-tags=integration','-coverpkg=./internal/...','-coverprofile='+str(E/'coverage.out'),'./...','-v','-count=1'])
run('final-vet',[GO,'vet','./...'])
run('final-module-verify',[GO,'mod','verify'])
run('final-tidy-diff',[GO,'mod','tidy','-diff'])
run('final-build-host',[GO,'build','-trimpath','-o','/tmp/goacos-single-active-host-20261011','.'])
run('final-build-linux-arm64',[GO,'build','-trimpath','-o','/tmp/goacos-single-active-linux-arm64-20261011','.'],{'GOOS':'linux','GOARCH':'arm64','CGO_ENABLED':'0'})
run('final-binary-toolchain',[GO,'version','-m','/tmp/goacos-single-active-linux-arm64-20261011'])
for name,args in [('govuln-source',[SCAN,'-json','./...']),('govuln-binary-host',[SCAN,'-json','-mode=binary','/tmp/goacos-single-active-host-20261011']),('govuln-binary-linux-arm64',[SCAN,'-json','-mode=binary','/tmp/goacos-single-active-linux-arm64-20261011'])]:
 text=run(name,args);records=[];d=json.JSONDecoder()
 while text.strip():
  v,i=d.raw_decode(text.lstrip());records.append(v);text=text.lstrip()[i:]
 findings=[v['finding'] for v in records if 'finding'in v]
 called=[v for v in findings if any(f.get('function')for f in v.get('trace',[]))]
 summary={'record_count':len(records),'osv_ids':sorted({v['osv']['id']for v in records if 'osv'in v}),'finding_records':len(findings),'called_records':len(called),'findings':findings,'interpretation':'JSON exit0 is not interpreted as clean; finding records and trace functions separately counted'}
 (E/(name+'.json')).write_text(json.dumps(summary,indent=2)+'\n');print(name,'findings',len(findings),'called',len(called),flush=True)
 if called:sys.exit(3)
