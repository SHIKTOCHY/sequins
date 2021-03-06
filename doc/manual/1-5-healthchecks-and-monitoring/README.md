# Healthchecks and Monitoring

### The Status Page and JSON

Visiting the host and port that a sequins node is running on with your browser
will give you a simple status page with sharding and version information for
each database:

![](status.png)

The information presented represents the whole cluster in the distributed case,
and should be the same no matter which node you ask. You can also ask for a
specific db, by visiting `/<db>` (in this example: `localhost:9590/flights`).

Finally, you can get a JSON representation of the same information by fetching
with `Accept: application/json`:

    $ http localhost:9590 'Accept:application/json'
    {
        "dbs": {
            "flights": {
              ...

### Expvars

You can bind the sequins ["debug" HTTP
server](../x-1-configuration-reference/README.md#debug) to a different port, and
it'll publish go "expvars" at `/debug/vars`.

In addition to the [built in expvars][goexpvar], sequins publishes the following
sequins-specific ones:

 - `sequins.Qps.ByStatus`: A map of HTTP status to a count of requests in the.
   past second

 - `sequins.Qps.Total`: A total of the above.

 - `sequins.Latency`: A histogram of latency values for the last second, with
   `Max`, `Mean`, and `PXX` keys.

 - `sequins.DiskUsed`: The amount of local storage used by sequins.

[goexpvar]: https://golang.org/pkg/expvar/

### Datadog

At Stripe, we use [Datadog][datadog] for statsd-like monitoring with lots of
bells and whistles. We've open-sourced our Datadog plugin for sequins on
[github][ddcheck].

[datadog]: https://www.datadoghq.com/
[ddcheck]: https://github.com/stripe/datadog-checks/blob/master/checks.d/sequins.py
