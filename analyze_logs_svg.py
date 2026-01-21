import re
import sys

def parse_logs(filename):
    timestamps = []
    # format: {'variant_name': {'x': [], 'y': [], 'acc': 'A100'}}
    replicas = {}
    
    # Regex for the allocation line
    alloc_pattern = re.compile(r"Desired Optimized Allocation for VA: (\S+) - Replicas: (\d+), Accelerator: (\S+)")
    
    with open(filename, 'r') as f:
        for line in f:
            match = alloc_pattern.search(line)
            if match:
                va_name = match.group(1)
                count = int(match.group(2))
                acc = match.group(3)
                
                if va_name not in replicas:
                    replicas[va_name] = {'x': [], 'y': [], 'acc': acc}
                
                replicas[va_name]['y'].append(count)
                replicas[va_name]['x'].append(len(replicas[va_name]['x'])) 

    return replicas

def write_svg(data, output_file, title):
    # Determine bounds
    all_x = []
    all_y = []
    for va, vals in data.items():
        all_x.extend(vals['x'])
        all_y.extend(vals['y'])
    
    if not all_x:
        print("No data to plot")
        return

    min_x, max_x = min(all_x), max(all_x)
    min_y, max_y = 0, max(all_y) + 2 # Add some headroom
    
    width = 800
    height = 500
    padding = 60
    
    # Scaling functions
    def scale_x(val):
        if max_x == min_x: return padding
        return padding + (val - min_x) / (max_x - min_x) * (width - 2*padding)
        
    def scale_y(val):
        if max_y == min_y: return height - padding
        return height - padding - (val - min_y) / (max_y - min_y) * (height - 2*padding)
        
    colors = ["#1f77b4", "#ff7f0e", "#2ca02c", "#d62728"]
    
    svg_content = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" style="background-color: white;">']
    
    # Title
    svg_content.append(f'<text x="{width/2}" y="30" text-anchor="middle" font-family="sans-serif" font-size="16">{title}</text>')
    
    # Axes
    svg_content.append(f'<line x1="{padding}" y1="{height-padding}" x2="{width-padding}" y2="{height-padding}" stroke="black" />') # X axis
    svg_content.append(f'<line x1="{padding}" y1="{height-padding}" x2="{padding}" y2="{padding}" stroke="black" />') # Y axis
    
    # Labels
    # X Axis Label
    svg_content.append(f'<text x="{width/2}" y="{height-10}" text-anchor="middle" font-family="sans-serif">Sampling Steps</text>')
    # Y Axis Label
    svg_content.append(f'<text x="15" y="{height/2}" text-anchor="middle" font-family="sans-serif" transform="rotate(-90 15,{height/2})">Replicas</text>')
    
    # Plot Lines
    color_idx = 0
    legend_y = 60
    
    for va, vals in data.items():
        color = colors[color_idx % len(colors)]
        label = f"{va} ({vals['acc']})"
        points = []
        for x, y in zip(vals['x'], vals['y']):
            sx = scale_x(x)
            sy = scale_y(y)
            points.append(f"{sx},{sy}")
            # Dot
            svg_content.append(f'<circle cx="{sx}" cy="{sy}" r="3" fill="{color}" />')
            
        polyline = f'<polyline points="{" ".join(points)}" fill="none" stroke="{color}" stroke-width="2" />'
        svg_content.append(polyline)
        
        # Legend
        svg_content.append(f'<rect x="{width-200}" y="{legend_y}" width="10" height="10" fill="{color}" />')
        svg_content.append(f'<text x="{width-180}" y="{legend_y+10}" font-family="sans-serif" font-size="12">{label}</text>')
        legend_y += 20
        color_idx += 1
        
    svg_content.append('</svg>')
    
    with open(output_file, 'w') as f:
        f.write("\n".join(svg_content))
    print(f"Generated {output_file}")

if __name__ == "__main__":
    if len(sys.argv) < 3:
        pass
        
    log_file = sys.argv[1]
    output_file = sys.argv[2]
    mode = "reactivity"
    if len(sys.argv) > 3:
        mode = sys.argv[3] # Used for title
        
    title_map = {
        "reactivity": "Experiment 1: Saturation Reactivity",
        "cost": "Experiment 2: Cost-Aware Scaling"
    }
    title = title_map.get(mode, mode)
    
    data = parse_logs(log_file)
    write_svg(data, output_file, title)
