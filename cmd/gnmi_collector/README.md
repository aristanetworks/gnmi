# gnmi_collector

`gnmi_collector` is a collector that supports gNMI dial-out via [gRPC tunnel](https://github.com/openconfig/grpctunnel).

The collector is a gRPC tunnel server listening for incoming gRPC tunnel requests from a target. When a tunnel connection is established with the gRPC tunnel client from the target, the collector sends a gNMI request via the gRPC tunnel.

## Installation

```
go install github.com/aristanetworks/gnmi/cmd/gnmi_collector@latest
```

## Example

```
$ cat config.cfg
request: <
  key: "sample_counter"
  value: <
    subscribe: <
      mode: STREAM
      prefix: <
        origin: "openconfig"
      >
      subscription: <
        mode: SAMPLE
        sample_interval: 1000000000
        path: <
          elem: <
            name: "interfaces"
          >
          elem: <
            name: "interface"
            key: <
              key: "name"
              value: "Management1"
            >
          >
          elem: <
            name: "state"
          >
          elem: <
            name: "counters"
          >
          elem: <
            name: "out-octets"
          >
        >
      >
    >
  >
>
```

```
$ gnmi_collector \
    -port 10000 \
    -config_file config.cfg \
    -tunnel_request sample_counter \
    -tunnel_ca tunnel_ca.crt \
    -tunnel_cert tunnel.crt \
    -tunnel_key tunnel.key \
    -gnmi_ca gnmi_ca.crt \
    -gnmi_cert gnmi.crt \
    -gnmi_key gnmi.key \
    -username admin \
    -password pass
```

The collector is configured with the following options:
* `gnmi_collector` listens for a gRPC tunnel request on port 10000.
* gNMI requests are defined in the configuration file `config.cfg`.
* The gNMI request to be sent via the tunnel is named `sample_counter`, which is keyed in `config.cfg`.
* The CA, cert and key are TLS related options for the tunnel connection and gNMI server. If no TLS options are specified, a plaintext connection is used.
* The username/password are used as credentials for the gNMI connection.

When a tunnel connection is established with the target and a gNMI request is sent via the tunnel, the collector will output updates.

```
2021/10/06 03:45:06 watching for tunnel connections
2021/10/06 03:45:06 adding target: {ID:id1 Type:GNMI_GNOI}
2021/10/06 03:45:06 Added target: "id1":&{conf:0xc0002ce4d0 conn:0xc0002bd890}
2021/10/06 03:45:07 &{timestamp:1633517107312763854  prefix:{target:"id1"}  update:{path:{elem:{name:"interfaces"}  elem:{name:"interface"  key:{key:"name"  value:"Management1"}}  elem:{name:"state"}  elem:{name:"counters"}  elem:{name:"out-octets"}}  val:{uint_val:3331454}}}
2021/10/06 03:45:08 &{timestamp:1633517108313200753  prefix:{target:"id1"}  update:{path:{elem:{name:"interfaces"}  elem:{name:"interface"  key:{key:"name"  value:"Management1"}}  elem:{name:"state"}  elem:{name:"counters"}  elem:{name:"out-octets"}}  val:{uint_val:3334720}}}
2021/10/06 03:45:09 &{timestamp:1633517109314493403  prefix:{target:"id1"}  update:{path:{elem:{name:"interfaces"}  elem:{name:"interface"  key:{key:"name"  value:"Management1"}}  elem:{name:"state"}  elem:{name:"counters"}  elem:{name:"out-octets"}}  val:{uint_val:3335373}}}
```
