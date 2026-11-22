# First region and server provider

Status: **Proposed, pending sign-off** (issue #6, decision D3). Facts were
checked on Nov 21, 2026. This record is a draft for the maintainers. It is not
a final decision.

## Decision to be made

Which rented bare-metal provider hosts the first Tachyon region, and in which
metro. The architecture ([Compute layer](../architecture.md#compute-layer))
wants single-socket AM5 hosts: AMD EPYC 4004/4005 for ECC memory (otherwise
Ryzen 9 9950X class), 128 to 192 GB RAM, two local NVMe drives and 10 to 25
GbE. Issue #6 lists Hetzner, Latitude.sh and OVHcloud as candidates.

## Recommendation

1. **Provider: Latitude.sh for the first region, if the maintainers confirm
   ECC with them in writing.** It is the only candidate with both EPYC
   4004/4005 plans and 2 x 10 Gbps public ports in the US and EU, a
   provisioning API with Terraform and CLI, and 25 locations to expand into.
   It costs roughly 1.5x OVHcloud for similar CPUs (for example $296 to $398 against $198 to $255), which we accept for a first
   region of a handful of hosts.
2. **OVHcloud Advance is the price and fallback option.** It is the cheapest
   EPYC 4004/4005 offer and has 25 Gbps private links, but public bandwidth
   is only 3 Gbps and the memory is labeled "On-Die ECC" only (see risks).
   Its setup fee equals one month's price.
3. **Hetzner is the best fit for a cheap EU lab or second region, not for the
   EPYC spec.** Its dedicated line has no EPYC 4004/4005 (AM5 Ryzen 9
   7950X3D with DDR5 ECC in the AX102, EPYC 9454P in the AX162), its
   dedicated servers exist only in Germany and Finland, and 10G is an
   add-on with a 20 TB egress cap.
4. **Region: see the conditional table below. It depends on where the pilots
   are and we do not know that.** Default to US-East (Ashburn or New York) if
   no pilot data arrives, since GitHub's runner traffic and most CI
   customers are US-centric. This default is a guess, not a finding.

If sign-off is slow, the work in issue #35 can start with the checklist at
the end: account, KYC and quota steps take days and do not depend on the
final provider.

## Region, conditional on pilot locations (open input)

**Open input for the maintainers:** where are the first 3 to 5 pilot
customers' people, repos and artifact registries? Decover is a possible
pilot, but its location is not assumed here.

| If the pilots are mostly... | Then pick | Candidates with that metro (per the sources below) |
| --- | --- | --- |
| US-East | Ashburn VA or New York | Latitude.sh (Ashburn, New York per aggregator list, UNVERIFIED per plan); OVHcloud Vint Hill VA (Advance) |
| US-West | Los Angeles or Hillsboro OR | Latitude.sh (Los Angeles, UNVERIFIED per plan); OVHcloud Hillsboro (Advance) |
| EU | Frankfurt, or Falkenstein/Nuremberg for Hetzner | Latitude.sh Frankfurt, Amsterdam, London; Hetzner DE/FI; OVHcloud Gravelines, Roubaix, Strasbourg, Limburg, Warsaw, Erith (UK) |
| Mixed US and EU | US-East first, EU as the second region | The architecture already plans EU data residency as the second region |
| Latin America or Asia-Pacific | Latitude.sh only among the three (Sao Paulo, Singapore, Tokyo, Sydney per aggregator list) | UNVERIFIED |

Latency matters less than it seems: a job's critical path is the pickup
(p95 under 10 seconds) and GitHub's own API round trips, not the distance to
the customer. What does depend on the metro is the distance to GitHub, to
the customers' registries and to each other's caches.

## Facts gathered

Every row names a source and the date checked. "Aggregator" means a third
party page, not the provider; treat those prices as indicative. Prices are
monthly, excluding VAT or tax, in the currency shown.

### Server models

| Provider | Model | CPU | RAM | Storage | Network | Price | Setup fee | Source (checked Nov 21, 2026) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Latitude.sh | m4.metal.small | EPYC 4244P, 6 cores, 3.8 GHz | 48 GB (aggregator says 64 GB: conflict) | 2 x 960 GB NVMe | 2 x 10 Gbps | $296 | UNVERIFIED | https://www.latitude.sh/pricing |
| Latitude.sh | f4.metal.small | EPYC 4484PX, 12 cores, 4.4 GHz | 96 GB | 2 x 960 GB NVMe | 2 x 10 Gbps | $398 | UNVERIFIED | https://www.latitude.sh/pricing |
| Latitude.sh | f4.metal.medium | EPYC 4564P, 16 cores, 4.5 GHz | 128 GB | 2 x 480 GB + 2 x 1.9 TB NVMe | 2 x 10 Gbps | $555 | UNVERIFIED | https://www.latitude.sh/pricing |
| Latitude.sh | m4.metal.medium | EPYC 9124 (not AM5), 16 cores | 128 GB | 2 x 480 GB + 2 x 1.9 TB NVMe | 2 x 10 Gbps | $456 | UNVERIFIED | https://www.latitude.sh/pricing |
| OVHcloud | Advance-2 (2024) | EPYC 4344P, 8c/16t | 64 GB DDR5-5200, "On-Die ECC" | 2 x 960 GB NVMe, soft RAID | 3 Gbps public, 25 Gbps private | $198 (US site) | $198 | https://us.ovhcloud.com/bare-metal/advance/ |
| OVHcloud | Advance-3 (2024) | EPYC 4464P, 12c/24t | 64 GB DDR5-5200, "On-Die ECC" | 2 x 960 GB NVMe | 3 Gbps public, 25 Gbps private | $255 (US site; EU blog says the 2026 generation Advance-3 is EUR 259.99) | $255 | https://us.ovhcloud.com/bare-metal/advance/ |
| OVHcloud | Advance-4 (2024) | EPYC 4584PX, 16c | 64 GB | 2 x 960 GB NVMe | 25 Gbps private (public UNVERIFIED) | EUR 254.99 from Aug 11, 2026 (was EUR 219.99) | UNVERIFIED | https://blog.ovhcloud.com/en/posts/dedicated-servers-pricing-update-h2-2026/ |
| OVHcloud | Advance-4 (2026) | EPYC 4585PX, 16c | 64 GB | 2 x 960 GB NVMe | UNVERIFIED | EUR 299.99 from Aug 11, 2026 | UNVERIFIED | https://blog.ovhcloud.com/en/posts/dedicated-servers-pricing-update-h2-2026/ |
| Hetzner | AX102-3 | Ryzen 9 7950X3D, 16c (AM5, not EPYC) | 128 GB DDR5 ECC | 2 x 1.92 TB NVMe (DC) | 1 Gbps, 10G uplink add-on supported | EUR 159 to 259 range (per config) | EUR 129 | https://www.hetzner.com/dedicated-rootserver/matrix-ax/ and https://docs.hetzner.com/robot/dedicated-server/server-lines/ax-server/ |
| Hetzner | AX42-2 | Ryzen 7 PRO 8700GE, 8c | 64 GB DDR5 ECC | 2 x 512 GB NVMe | 1 Gbps, no 10G add-on | EUR 79 to 99 range | EUR 49 | same two Hetzner pages |
| Hetzner | AX162-1 | EPYC 9454P, 48c (SP5, not AM5) | 128 GB DDR5 ECC reg. | 2 x 3.84 TB NVMe (DC) | 10G add-on supported | from EUR 319 (page) or EUR 612.30 (a 2026 blog; conflict) | EUR 304 | same two Hetzner pages |
| Hetzner | EPYC 4004/4005 model | none listed | n/a | n/a | n/a | n/a | n/a | https://docs.hetzner.com/robot/dedicated-server/server-lines/ax-server/ (the docs page may be about a year old) |

Notes on ECC. The architecture asks for ECC. Only Hetzner states "DDR5 ECC"
for its AM5 models. OVHcloud labels its Advance memory "On-Die ECC", which
is the DDR5 chip-internal correction every DDR5 module has; it does not say
whether the DIMMs carry end-to-end ECC. Latitude.sh's pricing page says
nothing on ECC, and one review (https://hostline.io/blog/bare-metal-gpu-servers-for-ai-workloads-2026/)
claims "System RAM ECC support" without a spec. Both are **UNVERIFIED** until
the provider confirms in writing.

### Network, bandwidth and egress

| Provider | Fact | Source (checked Nov 21, 2026) |
| --- | --- | --- |
| Latitude.sh | 20 TB free outbound per server per month, unlimited inbound; extra egress $10/TB ($0.01/GB), cheaper in prepaid 10 TB packs | https://www.latitude.sh/pricing and https://getdeploying.com/latitude-sh |
| Latitude.sh | Fair Usage Policy: an account over its FUP threshold, or an attack from a device, can be null-routed (DDoS null-route usually two hours). The numeric threshold is not published (UNVERIFIED) | https://www.latitude.sh/legal/fair-usage |
| OVHcloud | Advance: 3 Gbps public, unmetered and guaranteed; 25 Gbps private. No 10 Gbps option or upgrade price shown. Public 1 to 5 Gbps by model | https://us.ovhcloud.com/bare-metal/advance/ and https://www.ovhcloud.com/en/bare-metal/advance/ |
| Hetzner | Standard 1 Gbps, unlimited traffic. With the 10G uplink: 20 TB outbound included, EUR 1/TB over, inbound and internal unlimited, no bandwidth cap. Moving an existing server: EUR 32.77, or EUR 99 with a rebuild | https://docs.hetzner.com/robot/dedicated-server/network/10g-uplink/ and https://www.hetzner.com/dedicated-rootserver/matrix-ax/ |
| Hetzner | 10G uplink monthly fee: a 2024 forum comment says EUR 42.90 including NIC; not from Hetzner. UNVERIFIED | https://news.ycombinator.com/item?id=41182262 |
| Hetzner | A 10G order counts as customized and takes at least 2 to 3 business days (reported by a search summary of Hetzner docs; page not re-read) | https://docs.hetzner.com/general/infrastructure-and-availability/order-processing/ |

### Locations

| Provider | Locations | Source (checked Nov 21, 2026) |
| --- | --- | --- |
| Latitude.sh | "25 locations across 5 continents". An aggregator lists Dallas, Los Angeles, New York, Chicago, Ashburn, Miami, London, Frankfurt, Amsterdam, Sao Paulo, Mexico City, Buenos Aires, Bogota, Santiago, Singapore, Tokyo, Sydney. Which 4004/4005 plan is offered where: UNVERIFIED (one aggregator shows the 4244P plan in Toronto, Frankfurt, Amsterdam and others; the 4484PX plan in Toronto, Frankfurt, London) | https://www.latitude.sh/ , https://linuxdork.com/hosting/dedicated/latitude-sh/ , https://www.serversearcher.com/servers/provider/latitudesh/ |
| OVHcloud | Advance: Gravelines, Roubaix, Strasbourg (FR), Erith (UK), Limburg (DE), Warsaw (PL), Hillsboro OR and Vint Hill VA (US). Scale also Montreal. Asia-Pacific section not read | https://us.ovhcloud.com/bare-metal/regions-availability/ |
| Hetzner | Dedicated servers: Nuremberg, Falkenstein (DE), Helsinki (FI) only. Ashburn, Hillsboro and Singapore are cloud-only; dedicated root servers cannot be ordered there | https://docs.hetzner.com/cloud/general/locations/ |

### Ordering, API and lead time

| Provider | Fact | Source (checked Nov 21, 2026) |
| --- | --- | --- |
| Latitude.sh | Dashboard, REST API, CLI and Terraform provider; described as API-first. Deploy time and setup fee not stated anywhere I could read: UNVERIFIED | https://getdeploying.com/latitude-sh |
| OVHcloud | Cart-based ordering API at api.ovh.com. Marketing says delivery in minutes to an hour; the contract allows up to 15 working days from payment, and forum posts report multi-day waits for configured servers. Treat lead time as UNVERIFIED | https://us.ovhcloud.com/about/server-production/ and https://community.ovhcloud.com/t/ordering-follow-up/53709 |
| Hetzner | Robot webservice API: order endpoint takes product, distribution and location (server auction has its own). Setup fee applies per model (see above). Lead time for standard orders UNVERIFIED | https://robot.hetzner.com/doc/webservice/en.html |

### Pricing changes to watch

OVHcloud raised Advance prices on Aug 11, 2026 for new orders (for example
Advance-1 2024 from EUR 89.99 to 114.99, Advance-4 2024 from 219.99 to
254.99) and from Oct 1, 2026 for renewals. Hetzner changed its dedicated
line to fixed variants on Jun 15, 2026, according to one blog (UNVERIFIED,
single secondary source: https://wz-it.com/en/blog/hetzner-cloud-vs-dedicated-server/ ).
Prices on all three have moved this year. Re-quote before ordering.

### Terms that matter for running other people's CI

Tachyon runs customers' arbitrary code, including the abuse CI platforms
usually attract (crypto mining, port scanning, outbound floods). We need a
provider that tolerates that and will contact us before cutting a host off.

| Provider | Fact | Source (checked Nov 21, 2026) |
| --- | --- | --- |
| Hetzner | The dedicated server agreement prohibits crypto mining, scanning foreign networks, changing the MAC address and fake source IPs. Resale or hosting third parties: the page does not mention it, so UNVERIFIED. Abuse reports come through abuse.hetzner.com | https://www.hetzner.com/legal/dedicated-server/ |
| Latitude.sh | FUP and null-routing as above. Governed by the Megaport Global Services Agreement per one review; I did not read the current terms. Multi-tenant hosting of customer code: UNVERIFIED | https://www.latitude.sh/legal/fair-usage and https://www.whtop.com/review/latitude.sh |
| OVHcloud | Specific Conditions for Dedicated Servers PDF found but not read. UNVERIFIED | https://storage.gra.cloud.ovh.net/v1/AUTH_325716a587c64897acbef9a4a4726e38/contracts/0e186cd-contrat_partDedie-GB-10.0.pdf |

Whichever provider wins, the maintainers should ask in writing: may we run
untrusted third-party workloads in microVMs, what happens on an abuse
report (notice first or immediate null-route), and what is the outbound
traffic policy for a CI fleet.

## Other providers noticed

Not evaluated in depth. Marketing pages only; confirm every figure.

| Provider | Fact | Source (checked Nov 21, 2026) |
| --- | --- | --- |
| Vastrox | Advertises AMD Ryzen and EPYC bare metal with DDR5 ECC, 25 GbE, EPYC 4585PX option, European datacenters. Terms unreviewed | https://vastrox.com/dedicated-servers |
| InMotion Hosting | EPYC 4545P with 192 GB DDR5 ECC, up to 10 Gbps | https://www.inmotionhosting.com/dedicated-servers/amd-servers |
| ReliableSite | EPYC 4545P from $199/month, 1 TB NVMe, 5 TB monthly bandwidth cap at that tier | https://www.reliablesite.net/dedicated-servers/16-Core-server/amd-epyc-4545p-128GB |
| OrbitServers | EPYC 4565P from Frankfurt, "up to 100 Gbps" claimed | https://orbitservers.io/bare-metal |
| XLC | EPYC 4585PX, 64 GB DDR5 ECC, Los Angeles, 250 Mbps bandwidth on one listing | https://xlc.com/dedicated-servers/bare-metal-servers/ |

Equinix Metal, Leaseweb, Cherry Servers, phoenixNAP, Scaleway Elastic Metal
and Servers.com were not researched. Worth one quote each if neither
Latitude.sh nor OVHcloud confirms ECC.

## Criteria and weighting

| Criterion | Weight | Latitude.sh | OVHcloud Advance | Hetzner |
| --- | --- | --- | --- | --- |
| Meets host spec (AM5 EPYC 4004/4005 with real ECC) | 25% | EPYC yes, ECC UNVERIFIED | EPYC yes, ECC unclear ("On-Die") | ECC yes, no EPYC 4004/4005 |
| Network: 10 to 25 GbE public, egress terms | 20% | Best: 2 x 10 Gbps, 20 TB included | 3 Gbps public, 25 Gbps private only | 1 Gbps, 10G add-on, 20 TB included |
| Fits pilot region | 20% | Many US and EU metros | US (2), EU, UK | EU only |
| Price and setup fee | 15% | Highest; setup fee UNVERIFIED | Cheapest per core; setup fee = 1 month | Cheapest overall; modest setup fee |
| Provisioning API and lead time | 10% | API, CLI, Terraform | API; lead time uncertain | API; 2 to 3 days for 10G |
| Terms for hosting customer CI code | 10% | UNVERIFIED | UNVERIFIED | Prohibits mining and scanning |

I have not scored cells numerically because too many are UNVERIFIED; the
table shows where each provider leads. Latitude.sh leads on the first two
rows weighted highest after spec (network and region). If ECC is not
confirmed there, OVHcloud Advance and Latitude.sh tie on spec and the
decision moves to the terms rows. Hetzner wins on price and is the safest
on documented ECC, but only for EU.

## Risks

- **ECC may not be there.** The Compute layer requires ECC. If neither
  Latitude.sh nor OVHcloud confirms ECC UDIMMs on the 4004/4005 plans, fall
  back to the Hetzner AX102 (documented DDR5 ECC) in the EU, or relax the
  requirement and accept a Ryzen 9 9950X class host without ECC for the
  private beta, as the architecture already allows.
- **Terms may forbid what we do.** Running third-party code is the product.
  Get the answer in writing before ordering more than one host.
- **Price volatility.** OVHcloud and Hetzner both raised or restructured
  prices in 2026. Quotes in this record will go stale.
- **Egress and null-routing.** CI fleets pull large images; a 20 TB cap and a
  FUP null-route can look like an outage. Mirrors (see the architecture) help.
- **Provider lock-in is small.** hostd is network-booted, so moving hosts is
  a provisioning change. The cost of switching is the setup fee and a
  migration of the base image and prebuild cache.
- **Single provider, single region.** The reliability section expects
  overflow to the next region. A one-region start has no spill target; the
  second region should be a different provider or metro.

## What would change the decision

- Pilot customers are mostly in the EU: Hetzner (AX102 for cost) or
  Latitude.sh Frankfurt moves up, and OVHcloud Gravelines or Limburg is the
  price option.
- Latitude.sh cannot confirm ECC or a setup fee and deploy time within a
  week: choose OVHcloud Advance (EU or Vint Hill) or Hetzner.
- OVHcloud confirms full ECC DIMMs and a public port above 3 Gbps: it
  becomes the cheaper first choice.
- A provider's terms exclude running customers' untrusted code: remove it.
- Prices differ materially from this record when re-quoted.
- Load becomes predictable (the architecture's trigger for colocation):
  revisit with a colocation quote.

## Next steps for the operator once a provider is chosen

This lets fleet provisioning (issue #35) start. None of it has been done.

- [ ] Open the account under the company, add billing, and finish any
      identity, business or fraud verification (KYC). Expect days for a new
      account at any of the three.
- [ ] Ask support for server quota and confirm the number of 4004/4005 hosts
      available in the chosen metro (stock, not just the catalog).
- [ ] Get written answers on ECC, the acceptable-use terms for customer CI
      code, abuse-report handling and egress policy.
- [ ] Get the setup fee and delivery time in writing for the exact plan and
      location. Order one host first as a canary.
- [ ] Create an API credential scoped to provisioning, store it in the key
      store (see 0002-key-store.md), and verify list, order and reinstall
      calls with a test server.
- [ ] Confirm out-of-band access: IPMI/BMC or KVM-over-IP, and how to reach
      it (VPN, whitelisted IP or a time-limited session).
- [ ] Confirm network boot (PXE or iPXE, or a custom OS image) is allowed,
      since hosts are provisioned from code and network-booted.
- [ ] Confirm private networking or VLANs between hosts, and the public IP
      count and cost.
- [ ] Record the chosen plan, location and quotes in this file and change
      the status to **decided**.

## Not yet done

- Sign-off by the maintainers, and the pilot-location input.
- Written confirmations from providers listed above (ECC, setup fees, lead
  times, terms).
- Re-checking Hetzner's current EX and AX pages directly. The fetched
  Hetzner pages did not list every model, and one is about a year old.
