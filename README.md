# Nearby Cities

Inspired by my exploration of [geohash](https://github.com/quantonganh/geohash), I wondered if there's a Latitude/Longitude database and I stumbled upon [this](https://simplemaps.com/data/world-cities) gem. 

Imagine the possibilities: on weekends, when you're itching for a getaway near a city, this database can be your compass, helping you discover cities within a 100km radius.

```sh
$ http get 'http://localhost:8080/v1/cities/nearby?latitude=21.0278&longitude=105.8342&radius=100'
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8
Date: Tue, 12 Dec 2023 10:21:23 GMT
Transfer-Encoding: chunked

[
    {
        "name": "Hanoi",
        "lat": 21.0283,
        "lng": 105.8542,
        "country": "Vietnam",
        "geohash": "w7er8u0dtxhr"
    },
    {
        "name": "Haiphong",
        "lat": 20.8651,
        "lng": 106.6838,
        "country": "Vietnam",
        "geohash": "w7eyewku08wc"
    },
    {
        "name": "Bắc Ninh",
        "lat": 21.1833,
        "lng": 106.05,
        "country": "Vietnam",
        "geohash": "w7g2t0r38j76"
    },
```
