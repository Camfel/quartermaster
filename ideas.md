
- quartermaster component repo
    - a repo that simply hosts component-stack.yaml files so that when the compoenent is enabled in quaretemaster it pulls from here. we need to think about how we want to version this, do only run a latest that uses latest images or do we rollout tags or releases.
- GUI container in seperate repo to show status of services running, possible restart button.
- support codeberg and github repos, along with deploy ssh keys etc.
- Load balancer component
    - Looks at services enabled with load balancing and creates a single entry point to the host and manages the routing of traffic to the right services.
    - should allow for DNS and optional public dynamic DNS service routing, example jellyfin.<custom-dns> to be routed to the jellyfin container/service.

- Container repo component
- back up component
- llama cpp compoent
- agent component
  - able to run pentest
    - possible add to github CI checks before merging a PR to qm itself
  - able to connect to a local or llm via openAI protocols
  - Able to look at a random repo and create a component from there, maybe as a skill.md
  - Able to run on schedule or get started when health checks fail so it can intervene and fix the problem
