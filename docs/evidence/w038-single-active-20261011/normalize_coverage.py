#!/usr/bin/env python3
"""Union identical statement blocks from cross-package coverpkg output."""
import pathlib,json,collections
E=pathlib.Path(__file__).resolve().parent
blocks={};duplicates=0
for line in (E/'coverage.out').read_text().splitlines()[1:]:
 loc,n,count=line.split();n,count=int(n),int(count)
 if loc in blocks:
  duplicates+=1;assert blocks[loc][0]==n
  blocks[loc][1]=max(blocks[loc][1],count)
 else:blocks[loc]=[n,count]
(E/'coverage-normalized.out').write_text('mode: atomic\n'+''.join(f'{k} {n} {c}\n'for k,(n,c)in sorted(blocks.items())))
files=collections.defaultdict(lambda:[0,0])
for loc,(n,c)in blocks.items():f=loc.split(':')[0];files[f][1]+=n;files[f][0]+=n*(c>0)
primary=['internal/storage/config_owner.go','internal/storage/config_facade.go','internal/core/hub.go','internal/api/config_admission.go']
selected=[]
for name in primary:
 key='github.com/halfking/goacos/'+name;c,total=files[key];selected.append({'file':name,'covered_statements':c,'statements':total,'percent':round(100*c/total,2)})
c=sum(x['covered_statements']for x in selected);n=sum(x['statements']for x in selected)
summary={'raw_duplicate_identical_blocks':duplicates,'normalization':'one source-block location counted once; union of real cross-package executions, max counter; statement counts asserted identical','scope':'four complete primary files implementing new ownership/config facade, replacement hub, and HTTP admission; not repository/global/auth gate coverage','primary_files':selected,'primary_percent':round(c*100/n,2),'whole_repository_instrumented_percent':round(100*sum(v[0]for v in files.values())/sum(v[1]for v in files.values()),2)}
(E/'coverage-summary.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary,indent=2));assert all(x['percent']>=80 for x in selected)
