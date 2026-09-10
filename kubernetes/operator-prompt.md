
We are on a new branch of https://github.com/mostlygeek/llama-swap that will take on the ambitious task of turning it into a kubernetes "operator" of sorts.

We are also going to look into how we can do sticky sessioning against llama.cpp as paddler does when managing multiple instances on the back end.

This project is borne from the frustrations of putting llama.cpp into kserve, and having it handling llama.cpp very poorly overall.

I use the operator term losely, as for now I am NOT looking at anything involving a CRD, but it WILL be responsible for managing llama.cpp (and/or others).
It will be designed to run in a dedicated namespace, which it manages the resources in, discovers remenants, and cleans up old resources.

LLama-swap already does a great job of mapping many models in a catalog, running one or more of them based on rules, and swapping them out.

It however does NOT try to do any slot awareness or loadbalancing as of yet (though some inroads may be there for the latter). Regardling slot awareness,
we may want to analyze Paddler (https://github.com/intentee/paddler) which DOES have slot aware routing and kvcache-preserving behaviour.
Initially load-balancing will be OUT of scope, one of any given model can get it's llama.cpp launched, all traffic to that model goes to the one instance.
Managing multiple instances of a given model can come later. (This is mostly for smaller homelab setups, so the one may be fine)
Note that slot allocation and routing is seperate from loadbalancing, but loadbalancing would also need to take slots into account, as we need some level of stickiness.
Likewise even a single backend can benefit from explicit slot allocation/routing/saving. 

LLama-swap supports today three main types of backends:

1) Process launched (It runs a commandline for llama.cpp)
2) Docker laucnhed (It docker runs containers to launch models)
  - They have an "official" docker image which combines llama-swap with various backends for desktops
3) "Peer", which is proxying to a remote system.

The idea is the kubernetes adds a 4th type, which is sort of a blend of of #2 and #3 above.

Deployment launches the head-end/operator pods. There is NO support for multiple instances to start, but we should try to identify any
state that would need to be shared should we want to support that later and flag it as such in our planning.

When a request comes in, llama-swap check if there is a backend for it, if so, forwards the traffic to it. If not, provisions a
deployment and service from a template for the backend pod. (IE: llama.cpp, sd.cpp, crispasr). Take a lighteight approach for now,
try to be similar to the docker support, except enabled through a deployment/service provisioning on the kubernetes control plane in the given namespace.

When llama-swap decides it's time to shut it down, it simply deletes the resources from the namespace it manages for that model (the deployment/servie objects)

The template should allow rich configurability, akin to feeding values to a helm chart for customization. (IE: setting gpu in resources, or configuing volumes
for locally stored models, kv-caches) (maybe an option for a PVC for any given model's kv-cache, or local model cache if desired. If KV-cache PV is used, needs statefulset when balancing becomes in scope)

Rough order of implementation as I see it (but open to change if we need):

1) "Just llama-swap docker but in the kube"
  - get the basic mechanics of launch/teardown/API steering working
  - see if existing scheduler's (matix, group) work for the kube

2) "Smart slot handling"
  - every session derives some kind of unique ID, possibly based on the incoming prompt.
  - this gets set as a header when proxying back to llama.cpp
  - incoming connections will try to load it's slot id via API call to llama.cpp before forwarding
  - ending client connectiosn will cause an explicit slot save API call to llama.cpp 
  - An implementation of a wrapper for llama-swap that was discussed lives in a gist:
    - https://gist.github.com/urmuzov/c68ce96f6dbb1024f913f5a953182e2d
    - perhaps we can take some ideas from there.

3) "Load balancing of a model"
  - Builds on BOTH items above to:
    - allow the launcher to start more than 1 pod if configuration allows and load demands
      - llama-swap should know # of connections, and has a way to set limits already, aligned to --parallel llama.cpp flag
    - extend slot management to span slots across multiple instances, and ensures the right slot goes to the right backend
      - in an exceptional case like "backend scaled down", the slot can be re-assigned to a new node 

4) "Packaging"
  - We should have a helm chart
  - If we need a differentiated dockerfile from the project's main one, we should include it's build here
    - If possible however, maybe try to use existing docker images the project maintains
    - Custom images should always of course be possible

5) High Availability
  - How we run redundant head ends/operators, pick leaders, share state, do failover, etc.

In as much as possible, we should try, first, to make this a natural extension of llama-swap that could even see upstreaming.

Let us be clear this is first pass is to be basic, and simple, above all. Later we can look at things like how to share state
for leader elections, or multiple head ends, or keeping all our state in CRDs but not now.

For now do a lot of analysis of the ask, take a lot of notes, build a phased plan out and save it to kubernetes/PLAN.md for us to discuss
before any implementation begins. Feel free to build multip/many documents in that folder for now as we go, I encourage it.

That is also the folder for any testing/example manifests/configs and later helm chart.

For testing, there is a kube context available and a 'llama-swap' namespace. Stick to tiny models from huggingface for now. (I'll wire
in our model store as templates evolve)
