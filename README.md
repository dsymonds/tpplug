# TPPlug

This is a monitoring exporter for TP-Link smart plugs I own.

It works with at least these devices:
   * [TP-Link HS110](https://www.tp-link.com/au/home-networking/smart-plug/hs110/)
   * [TP-Link KP115](https://www.tp-link.com/au/home-networking/smart-plug/kp115/)

It can export data for use by [Prometheus](https://prometheus.io/).

## The protocol

https://github.com/plasticrake/tplink-smarthome-api was a good starting point.
https://www.softscheck.com/en/reverse-engineering-tp-link-hs110/ also had some
useful info.

