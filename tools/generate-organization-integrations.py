"""Update the hand-maintained Organization structural schema and chart copy."""
import json
from pathlib import Path
p=Path('config/crd/aep.aenix.io_organizations.yaml')
d=json.loads(p.read_text())
s=d['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']
props=s['spec']['properties']
ref={'type':'object','required':['name'],'properties':{'name':{'type':'string','minLength':1,'maxLength':253,'pattern':r'^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$'}}}
props['vcs']={'type':'object','required':['provider','url','credentialsSecretRef'],'properties':{
 'provider':{'type':'string','enum':['gitlab','github','gitea']},
 'url':{'type':'string','pattern':r'^https://[a-zA-Z0-9.-]+(:[0-9]+)?/?$'},
 'groupPath':{'type':'string','pattern':r'^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$'},
 'org':{'type':'string','pattern':r'^[a-zA-Z0-9_-]+$'},
 'ensureGroup':{'type':'boolean'},'credentialsSecretRef':ref,'caBundleSecretRef':ref},
 'x-kubernetes-validations':[
 {'rule':"self.provider == 'gitlab' ? has(self.groupPath) && !has(self.org) : has(self.org) && !has(self.groupPath)",'message':'Use groupPath for GitLab and org for GitHub/Gitea'},
 {'rule':"!has(self.ensureGroup) || !self.ensureGroup || self.provider == 'gitlab'",'message':'Only GitLab supports group creation'}]}
tiers=props['tiers']
# Existing schema uses named tier properties.
argocd={'type':'object','required':['enabled'],'properties':{'enabled':{'type':'boolean'},'version':{'type':'string','enum':['9.7.1']}}}
for tier in ('nonprod','prod'):
 tiers['properties'][tier]['properties']['argocd']=argocd
 if tier=='prod':
  tiers['properties'][tier]['x-kubernetes-validations']=[v for v in tiers['properties'][tier].get('x-kubernetes-validations',[]) if 'Argo CD' not in v.get('message','')]
  rule={'rule':"!has(self.argocd)",'message':'Argo CD is supported only in nonprod; use OpenChoreo promotion for prod'}
  if rule not in tiers['properties'][tier]['x-kubernetes-validations']:tiers['properties'][tier]['x-kubernetes-validations'].append(rule)
rule={'rule':"!has(self.tiers.nonprod.argocd) || !self.tiers.nonprod.argocd.enabled || has(self.vcs)",'message':'Argo CD requires spec.vcs'}
s['spec'].setdefault('x-kubernetes-validations',[])
if rule not in s['spec']['x-kubernetes-validations']:s['spec']['x-kubernetes-validations'].append(rule)
s['status']['properties']['argocdURL']={'type':'string'}
def lists(v):
 if isinstance(v,dict):
  v.pop('uniqueItems',None)
  if v.get('type')=='array':v.setdefault('x-kubernetes-list-type','atomic')
  for x in v.values():lists(x)
 elif isinstance(v,list):
  for x in v:lists(x)
lists(d)
text=json.dumps(d,indent=2)+'\n'
p.write_text(text)
Path('charts/aep-operator/crds/aep.aenix.io_organizations.yaml').write_text(text)
