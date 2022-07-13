#!/usr/bin/env python3

import paho.mqtt.client as mqtt
import json
import time
import threading
import uuid
import re
import yaml
import sys
from prometheus_client import start_http_server, Gauge

class Client:
    def __init__(self, host="", port="", user="", password="", topic=""):
        self.host = host
        self.port = port
        self.user = user
        self.password = password
        self.topic = topic
        self.uuid = str(uuid.uuid4())

    def connect(self, on_message):
        c = mqtt.Client(client_id=f"mqtt2prom-{self.uuid}")
        c.username_pw_set(self.user, self.password)
        c.on_connect = self.on_connect
        c.on_message = on_message
        c.connect(self.host, self.port, 60)
        self.client = c

    def on_connect(self, client, userdata, flags, rc):
        print("Connected with result code "+str(rc))
        client.subscribe(self.topic)

class Exporter:
    def __init__(self, metrics):
        self.metrics = {}
        for metric in metrics:
            name = metric['name']
            description = metric['description']
            labels = metric['labels']
            labels.sort()
            self.metrics[name] = {
                'prom': Gauge(name, description, labels),
                'topics': metric['topics'],
                'labels': labels,
                'label_mapping': metric.get('label_mapping', {})
            }

    def mqtt_handler(self, client, userdata, msg):
        for key, data in self.metrics.items():
            for topic in data['topics']:
                if topic.get('topic_regex', False):
                    topic_regex = re.compile(topic['topic'])
                    matched = topic_regex.match(msg.topic)
                    if matched:
                        value = msg.payload.decode()
                        labels = {}

                        for match_key, match_value in matched.groupdict().items():
                            if match_key.startswith('label__'):
                                labels[match_key.split('__')[1]] = match_value

                        if data['label_mapping']:
                            for label, label_config in data['label_mapping'].items():
                                regex_group = label_config['regex_group']
                                label_matched = matched[f"label__{regex_group}"]
                                mapping = label_config['values'].get(label_matched)
                                if not mapping:
                                    next
                                labels[label] = mapping

                        if all(l is not None for l in list(labels.values())):
                            data['prom'].labels(*labels.values()).set(value)
                            

                else:
                    if msg.topic == topic['topic']:
                        value = msg.payload.decode()
                        data['prom'].labels(*(topic['labels'].values())).set(value)

if __name__ == "__main__":
    config_file = sys.argv[1]
    with open(config_file, "r") as stream:
        config = yaml.safe_load(stream)

    exporter = Exporter(config['metrics'])

    mqtt_opts = config['mqtt']
    main_client = Client(**mqtt_opts)
    main_client.connect(exporter.mqtt_handler)

    start_http_server(8000)
    main_client.client.loop_forever()
