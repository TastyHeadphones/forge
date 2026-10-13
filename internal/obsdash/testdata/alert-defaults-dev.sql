-- FrontendJSErrorSpike (frontend-js-errors) severity=warning for=10m interval=1m
SELECT ServiceName AS service,
       countIf(Timestamp >= now() - INTERVAL 10 MINUTE) AS value
FROM otel_traces
WHERE ServiceName = 'shop-frontend'
  AND Timestamp >= now() - INTERVAL 10 MINUTE - INTERVAL 60 MINUTE
  AND ResourceAttributes['deployment.environment.name'] = 'dev'
  AND ResourceAttributes['rum.sessionId'] != ''
  AND lower(StatusCode) = 'error'
  AND SpanAttributes['component'] NOT IN ('fetch', 'xml-http-request')
GROUP BY ServiceName
HAVING value >= 10
   AND value > 3 * countIf(Timestamp < now() - INTERVAL 10 MINUTE) * 10 / 60
ORDER BY service

-- ServiceErrorRateHigh (error-rate) severity=critical for=5m interval=1m
WITH series AS (
    SELECT ServiceName AS service,
           MetricName AS metric,
           Attributes['error_class'] AS error_class,
           arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((TimeUnix, Value)))) AS vals,
           any(AggregationTemporality) AS temporality
    FROM otel_metrics_sum
    WHERE MetricName IN ('rpc.server.calls', 'rpc.server.errors')
      AND ((AggregationTemporality = 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE) OR (AggregationTemporality != 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE - INTERVAL 2 MINUTE))
      AND ResourceAttributes['deployment.environment.name'] = 'dev'
      AND splitByChar('/', Attributes['procedure'])[2] IN ('billing.v1.BillingService', 'orders.v1.OrdersService')
    GROUP BY ServiceName, ResourceAttributes, Attributes, MetricName
), increase AS (
    SELECT service, metric, error_class,
           if(temporality = 1, arraySum(vals), arraySum((v, d) -> if(d < 0, v, d), vals, arrayDifference(vals))) AS n
    FROM series
)
SELECT service,
       sumIf(n, metric = 'rpc.server.errors' AND error_class NOT IN ('user', 'canceled')) / sumIf(n, metric = 'rpc.server.calls') AS value
FROM increase
GROUP BY service
HAVING sumIf(n, metric = 'rpc.server.calls') >= 20 AND value > 0.05
ORDER BY service

-- ServiceLatencyP95High (p95-latency) severity=warning for=10m interval=1m
WITH series AS (
    SELECT ServiceName AS service,
           ExplicitBounds AS bounds,
           arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((TimeUnix, arrayMap(c -> toFloat64(c), BucketCounts))))) AS snaps,
           any(AggregationTemporality) AS temporality
    FROM otel_metrics_histogram
    WHERE MetricName = 'rpc.server.duration'
      AND ((AggregationTemporality = 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE) OR (AggregationTemporality != 1 AND TimeUnix >= now() - INTERVAL 5 MINUTE - INTERVAL 2 MINUTE))
      AND ResourceAttributes['deployment.environment.name'] = 'dev'
      AND splitByChar('/', Attributes['procedure'])[2] IN ('billing.v1.BillingService', 'orders.v1.OrdersService')
    GROUP BY ServiceName, ResourceAttributes, Attributes, ExplicitBounds
), increase AS (
    SELECT service, bounds,
           if(temporality = 1,
              arrayReduce('sumForEach', snaps),
              arrayReduce('sumForEach', arrayMap((cur, prev) -> arrayMap((c, q) -> if(c < q, c, c - q), cur, prev),
                                                 arraySlice(snaps, 2), arraySlice(snaps, 1, length(snaps) - 1)))) AS inc
    FROM series
    WHERE length(snaps) >= 2 OR temporality = 1
), merged AS (
    SELECT service, bounds, sumForEach(inc) AS counts FROM increase GROUP BY service, bounds
)
SELECT service,
       quantilePrometheusHistogram(0.95)(le, cum) AS value,
       max(cum) AS requests
FROM (
    SELECT service,
           arrayJoin(arrayZip(arrayPushBack(bounds, inf), arrayCumSum(counts))) AS pair,
           pair.1 AS le, pair.2 AS cum
    FROM merged
)
GROUP BY service
HAVING requests >= 20 AND value > 1
ORDER BY service

-- ServiceSilent (service-silent) severity=warning for=0s interval=1m
WITH moves AS (
    SELECT service, arrayJoin(t) AS t
    FROM (
        SELECT ServiceName AS service,
               arraySort(x -> x.1, groupArray((TimeUnix, Value))) AS pts,
               arrayMap(x -> x.1, pts) AS ts,
               arrayMap(x -> x.2, pts) AS vs,
               if(any(AggregationTemporality) = 1, vs, arrayDifference(vs)) AS moved,
               arrayFilter((tm, d) -> d != 0, ts, moved) AS t
        FROM otel_metrics_sum
        WHERE MetricName = 'rpc.server.calls'
          AND TimeUnix >= now() - INTERVAL 24 HOUR - INTERVAL 10 MINUTE - INTERVAL 1 MINUTE
          AND ResourceAttributes['deployment.environment.name'] = 'dev'
          AND splitByChar('/', Attributes['procedure'])[2] IN ('billing.v1.BillingService', 'orders.v1.OrdersService')
        GROUP BY ServiceName, ResourceAttributes, Attributes
    )
)
SELECT service, dateDiff('second', max(t), now()) AS value
FROM moves
GROUP BY service
HAVING max(t) < now() - INTERVAL 10 MINUTE
   AND uniqExactIf(toStartOfHour(t), t < now() - INTERVAL 10 MINUTE) >= 18
ORDER BY service

